package deployer

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/rancher/norman/controller"
	alertutil "github.com/rancher/rancher/pkg/controllers/user/alert/common"
	"github.com/rancher/rancher/pkg/controllers/user/helm/common"
	monitorutil "github.com/rancher/rancher/pkg/monitoring"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/ref"
	"github.com/rancher/rancher/pkg/settings"
	v1 "github.com/rancher/types/apis/core/v1"
	v3 "github.com/rancher/types/apis/management.cattle.io/v3"
	projectv3 "github.com/rancher/types/apis/project.cattle.io/v3"
	"github.com/rancher/types/config"

	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	ServiceName             = "alerting"
	waitCatalogSyncInterval = 60 * time.Second
)

const (
	defaultGroupIntervalSeconds = 180
)

type AlertService struct {
	clusterName        string
	clusterLister      v3.ClusterLister
	catalogLister      v3.CatalogLister
	apps               projectv3.AppInterface
	oldClusterAlerts   v3.ClusterAlertInterface
	oldProjectAlerts   v3.ProjectAlertInterface
	clusterAlertGroups v3.ClusterAlertGroupInterface
	projectAlertGroups v3.ProjectAlertGroupInterface
	clusterAlertRules  v3.ClusterAlertRuleInterface
	projectAlertRules  v3.ProjectAlertRuleInterface
	projectLister      v3.ProjectLister
	namespaces         v1.NamespaceInterface
	templateLister     v3.CatalogTemplateLister
	appDeployer        *appDeployer
}

func NewService() *AlertService {
	return &AlertService{}
}

func (l *AlertService) Init(cluster *config.UserContext) {
	ad := &appDeployer{
		appsGetter:       cluster.Management.Project,
		namespaces:       cluster.Management.Core.Namespaces(metav1.NamespaceAll),
		secrets:          cluster.Core.Secrets(metav1.NamespaceAll),
		templateVersions: cluster.Management.Management.CatalogTemplateVersions(namespace.GlobalNamespace),
	}

	l.clusterName = cluster.ClusterName
	l.clusterLister = cluster.Management.Management.Clusters("").Controller().Lister()
	l.catalogLister = cluster.Management.Management.Catalogs(metav1.NamespaceAll).Controller().Lister()
	l.oldClusterAlerts = cluster.Management.Management.ClusterAlerts(cluster.ClusterName)
	l.oldProjectAlerts = cluster.Management.Management.ProjectAlerts(metav1.NamespaceAll)
	l.clusterAlertGroups = cluster.Management.Management.ClusterAlertGroups(cluster.ClusterName)
	l.projectAlertGroups = cluster.Management.Management.ProjectAlertGroups(metav1.NamespaceAll)
	l.clusterAlertRules = cluster.Management.Management.ClusterAlertRules(cluster.ClusterName)
	l.projectAlertRules = cluster.Management.Management.ProjectAlertRules(metav1.NamespaceAll)
	l.projectLister = cluster.Management.Management.Projects(cluster.ClusterName).Controller().Lister()
	l.apps = cluster.Management.Project.Apps(metav1.NamespaceAll)
	l.namespaces = cluster.Core.Namespaces(metav1.NamespaceAll)
	l.templateLister = cluster.Management.Management.CatalogTemplates(metav1.NamespaceAll).Controller().Lister()
	l.appDeployer = ad

}

func (l *AlertService) Version() (string, error) {
	catalogID := settings.SystemMonitoringCatalogID.Get()
	templateVersionID, _, err := common.ParseExternalID(catalogID)
	if err != nil {
		return "", fmt.Errorf("get system monitor catalog version failed, %v", err)
	}
	return templateVersionID, nil
}

func (l *AlertService) Upgrade(currentVersion string) (string, error) {
	templateVersionNamespace, systemCatalogName, _, templateName, _, err := common.SplitExternalID(settings.SystemMonitoringCatalogID.Get())
	if err != nil {
		return "", err
	}

	templateID := fmt.Sprintf("%s-%s", systemCatalogName, templateName)
	template, err := l.templateLister.Get(templateVersionNamespace, templateID)
	if err != nil {
		return "", errors.Wrapf(err, "get template %s failed", templateID)
	}
	newExternalID := fmt.Sprintf("catalog://?catalog=%s&template=%s&version=%s", systemCatalogName, templateName, template.Spec.DefaultVersion)

	newVersion, _, err := common.ParseExternalID(newExternalID)
	if err != nil {
		return "", err
	}

	appName, _ := monitorutil.ClusterAlertManagerInfo()
	//migrate legacy
	if !strings.Contains(currentVersion, "system-library-rancher-monitoring") {
		if err := l.migrateLegacyClusterAlert(); err != nil {
			return "", err
		}

		if err := l.migrateLegacyProjectAlert(); err != nil {
			return "", err
		}

		if err := l.removeLegacyAlerting(); err != nil {
			return "", err
		}
	}

	//upgrade old app
	defaultSystemProjects, err := l.projectLister.List(metav1.NamespaceAll, labels.Set(systemProjectLabel).AsSelector())
	if err != nil {
		return "", fmt.Errorf("list system project failed, %v", err)
	}

	if len(defaultSystemProjects) == 0 {
		return "", fmt.Errorf("get system project failed")
	}

	systemProject := defaultSystemProjects[0]
	if systemProject == nil {
		return "", fmt.Errorf("get system project failed")
	}
	app, err := l.apps.GetNamespaced(systemProject.Name, appName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return newVersion, nil
		}
		return "", fmt.Errorf("get app %s:%s failed, %v", systemProject.Name, appName, err)
	}
	newApp := app.DeepCopy()
	newApp.Spec.ExternalID = newExternalID
	newApp.Spec.Answers["operator.enabled"] = "false"

	if !reflect.DeepEqual(newApp, app) {
		// check cluster ready before upgrade, because helm will not retry if got cluster not ready error
		cluster, err := l.clusterLister.Get(metav1.NamespaceAll, l.clusterName)
		if err != nil {
			return "", fmt.Errorf("get cluster %s failed, %v", l.clusterName, err)
		}
		if !v3.ClusterConditionReady.IsTrue(cluster) {
			return "", fmt.Errorf("cluster %v not ready", l.clusterName)
		}

		systemCatalog, err := l.catalogLister.Get(metav1.NamespaceAll, systemCatalogName)
		if err != nil {
			return "", fmt.Errorf("get catalog %s failed, %v", systemCatalogName, err)
		}

		if !v3.CatalogConditionUpgraded.IsTrue(systemCatalog) || !v3.CatalogConditionRefreshed.IsTrue(systemCatalog) || !v3.CatalogConditionDiskCached.IsTrue(systemCatalog) {
			return "", fmt.Errorf("catalog %v not ready", systemCatalogName)
		}

		if _, err = l.apps.Update(newApp); err != nil {
			return "", fmt.Errorf("update app %s:%s failed, %v", app.Namespace, app.Name, err)
		}
	}
	return newVersion, nil
}

func (l *AlertService) migrateLegacyClusterAlert() error {
	oldClusterAlert, err := l.oldClusterAlerts.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("get old cluster alert failed, %s", err)
	}
	for _, v := range oldClusterAlert.Items {
		migrationGroupName := fmt.Sprintf("migrate-group-%s", v.Name)
		groupID := alertutil.GetGroupID(l.clusterName, migrationGroupName)

		name := fmt.Sprintf("migrate-%s", v.Name)
		newClusterRule := &v3.ClusterAlertRule{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: l.clusterName,
			},
			Spec: v3.ClusterAlertRuleSpec{
				ClusterName: l.clusterName,
				GroupName:   groupID,
				CommonRuleField: v3.CommonRuleField{
					DisplayName: v.Spec.DisplayName,
					Severity:    v.Spec.Severity,
					TimingField: v3.TimingField{
						GroupWaitSeconds:      v.Spec.InitialWaitSeconds,
						GroupIntervalSeconds:  defaultGroupIntervalSeconds,
						RepeatIntervalSeconds: v.Spec.RepeatIntervalSeconds,
					},
				},
			},
		}

		if v.Spec.TargetNode != nil {
			newClusterRule.Spec.NodeRule = &v3.NodeRule{
				NodeName:     v.Spec.TargetNode.NodeName,
				Selector:     v.Spec.TargetNode.Selector,
				Condition:    v.Spec.TargetNode.Condition,
				MemThreshold: v.Spec.TargetNode.MemThreshold,
				CPUThreshold: v.Spec.TargetNode.CPUThreshold,
			}
		}

		if v.Spec.TargetEvent != nil {
			newClusterRule.Spec.EventRule = &v3.EventRule{
				EventType:    v.Spec.TargetEvent.EventType,
				ResourceKind: v.Spec.TargetEvent.ResourceKind,
			}
		}

		if v.Spec.TargetSystemService != nil {
			newClusterRule.Spec.SystemServiceRule = &v3.SystemServiceRule{
				Condition: v.Spec.TargetSystemService.Condition,
			}
		}

		oldClusterRule, err := l.clusterAlertRules.Get(newClusterRule.Name, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("migrate %s:%s failed, get alert rule failed, %v", v.Namespace, v.Name, err)
			}

			if _, err = l.clusterAlertRules.Create(newClusterRule); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("migrate %s:%s failed, create alert rule failed, %v", v.Namespace, v.Name, err)
			}
		} else {
			updatedClusterRule := oldClusterRule.DeepCopy()
			updatedClusterRule.Spec = newClusterRule.Spec
			if _, err := l.clusterAlertRules.Update(updatedClusterRule); err != nil {
				return fmt.Errorf("migrate %s:%s failed, update alert rule failed, %v", v.Namespace, v.Name, err)
			}
		}
		legacyGroup := &v3.ClusterAlertGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      migrationGroupName,
				Namespace: l.clusterName,
			},
			Spec: v3.ClusterGroupSpec{
				ClusterName: l.clusterName,
				CommonGroupField: v3.CommonGroupField{
					DisplayName: "Migrate group",
					Description: "Migrate alert from last version",
					TimingField: v3.TimingField{
						GroupWaitSeconds:      v.Spec.InitialWaitSeconds,
						GroupIntervalSeconds:  defaultGroupIntervalSeconds,
						RepeatIntervalSeconds: v.Spec.RepeatIntervalSeconds,
					},
				},
				Recipients: v.Spec.Recipients,
			},
		}

		_, err = l.clusterAlertGroups.Create(legacyGroup)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("migrate failed, create alert group %s:%s failed, %v", l.clusterName, migrationGroupName, err)
		}
	}
	return nil
}

func (l *AlertService) migrateLegacyProjectAlert() error {
	oldProjectAlert, err := l.oldProjectAlerts.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("get old project alert failed, %s", err)
	}

	oldProjectAlertGroup := make(map[string][]v3.ProjectAlert)
	for _, v := range oldProjectAlert.Items {
		if controller.ObjectInCluster(l.clusterName, v) {
			oldProjectAlertGroup[v.Spec.ProjectName] = append(oldProjectAlertGroup[v.Spec.ProjectName], v)
		}
	}

	for projectID, oldAlerts := range oldProjectAlertGroup {
		_, projectName := ref.Parse(projectID)

		for _, v := range oldAlerts {
			migrationGroupName := fmt.Sprintf("migrate-group-%s", v.Name)
			groupID := alertutil.GetGroupID(projectName, migrationGroupName)

			migrationRuleName := fmt.Sprintf("migrate-rule-%s", v.Name)
			newProjectRule := &v3.ProjectAlertRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      migrationRuleName,
					Namespace: projectName,
				},
				Spec: v3.ProjectAlertRuleSpec{
					ProjectName: projectID,
					GroupName:   groupID,
					CommonRuleField: v3.CommonRuleField{
						DisplayName: v.Spec.DisplayName,
						Severity:    v.Spec.Severity,
						TimingField: v3.TimingField{
							GroupWaitSeconds:      v.Spec.InitialWaitSeconds,
							GroupIntervalSeconds:  defaultGroupIntervalSeconds,
							RepeatIntervalSeconds: v.Spec.RepeatIntervalSeconds,
						},
					},
				},
			}

			if v.Spec.TargetPod != nil {
				newProjectRule.Spec.PodRule = &v3.PodRule{
					PodName:                v.Spec.TargetPod.PodName,
					Condition:              v.Spec.TargetPod.Condition,
					RestartTimes:           v.Spec.TargetPod.RestartTimes,
					RestartIntervalSeconds: v.Spec.TargetPod.RestartIntervalSeconds,
				}
			}

			if v.Spec.TargetWorkload != nil {
				newProjectRule.Spec.WorkloadRule = &v3.WorkloadRule{
					WorkloadID:          v.Spec.TargetWorkload.WorkloadID,
					Selector:            v.Spec.TargetWorkload.Selector,
					AvailablePercentage: v.Spec.TargetWorkload.AvailablePercentage,
				}
			}

			oldProjectRule, err := l.projectAlertRules.GetNamespaced(projectName, newProjectRule.Name, metav1.GetOptions{})
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return fmt.Errorf("migrate %s:%s failed, get alert rule failed, %v", v.Namespace, v.Name, err)
				}

				if _, err = l.projectAlertRules.Create(newProjectRule); err != nil && !apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("migrate %s:%s failed, create alert rule failed, %v", v.Namespace, v.Name, err)
				}
			} else {
				updatedProjectRule := oldProjectRule.DeepCopy()
				updatedProjectRule.Spec = newProjectRule.Spec
				if _, err := l.projectAlertRules.Update(updatedProjectRule); err != nil {
					return fmt.Errorf("migrate %s:%s failed, update alert rule failed, %v", v.Namespace, v.Name, err)
				}
			}

			legacyGroup := &v3.ProjectAlertGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      migrationGroupName,
					Namespace: projectName,
				},
				Spec: v3.ProjectGroupSpec{
					ProjectName: projectID,
					CommonGroupField: v3.CommonGroupField{
						DisplayName: "Migrate group",
						Description: "Migrate alert from last version",
						TimingField: v3.TimingField{
							GroupWaitSeconds:      v.Spec.InitialWaitSeconds,
							GroupIntervalSeconds:  defaultGroupIntervalSeconds,
							RepeatIntervalSeconds: v.Spec.RepeatIntervalSeconds,
						},
					},
					Recipients: v.Spec.Recipients,
				},
			}

			legacyGroup, err = l.projectAlertGroups.Create(legacyGroup)
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create migrate alert group %s:%s failed, %v", legacyGroup.Namespace, legacyGroup.Name, err)
			}
		}
	}
	return nil
}

func (l *AlertService) removeLegacyAlerting() error {
	legacyAlertmanagerNamespace := "cattle-alerting"

	if err := l.namespaces.Delete(legacyAlertmanagerNamespace, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrap(err, "failed to remove legacy alerting namespace when upgrade")
	}
	return nil
}
