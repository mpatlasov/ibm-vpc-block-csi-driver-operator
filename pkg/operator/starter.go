package operator

import (
	"context"
	"fmt"

	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"

	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	applyopv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	opclient "github.com/openshift/client-go/operator/clientset/versioned"
	opinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/ibm-vpc-block-csi-driver-operator/assets"
	"github.com/openshift/ibm-vpc-block-csi-driver-operator/pkg/controller/secret"
	"github.com/openshift/ibm-vpc-block-csi-driver-operator/pkg/util"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivernodeservicecontroller"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// DUMMY CHANGE

const (
	encryptionKeyParameter = "encryptionKey"
	encryptedParameter     = "encrypted"
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset and informers
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, util.OperatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, util.OperatorNamespace, "", util.ConfigMapNamespace)
	secretInformer := kubeInformersForNamespaces.InformersFor(util.OperatorNamespace).Core().V1().Secrets()
	configMapInformer := kubeInformersForNamespaces.InformersFor(util.OperatorNamespace).Core().V1().ConfigMaps()
	nodeInformer := kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes()

	// Create apiextension client. This is used to verify is a VolumeSnapshotClass CRD exists.
	apiExtClient, err := apiextclient.NewForConfig(rest.AddUserAgent(controllerConfig.KubeConfig, util.OperatorName))
	if err != nil {
		return err
	}

	// Create config clientset and informer. This is used to get the cluster ID
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, util.OperatorName))
	configInformers := configinformers.NewSharedInformerFactory(configClient, util.Resync)

	// operator.openshift.io client, used for ClusterCSIDriver
	operatorClientSet := opclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, util.OperatorName))
	operatorInformers := opinformers.NewSharedInformerFactory(operatorClientSet, util.Resync)

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := opv1.GroupVersion.WithResource("clustercsidrivers")
	gvk := opv1.SchemeGroupVersion.WithKind("ClusterCSIDriver")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(
		clock.RealClock{},
		controllerConfig.KubeConfig,
		gvr,
		gvk,
		util.InstanceName,
		extractOperatorSpec,
		extractOperatorStatus,
	)
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	csiControllerSet := csicontrollerset.NewCSIControllerSet(
		operatorClient,
		controllerConfig.EventRecorder,
	).WithLogLevelController().WithManagementStateController(
		util.OperandName,
		false,
	).WithStaticResourcesController(
		"IBMBlockDriverStaticResourcesController",
		kubeClient,
		dynamicClient,
		kubeInformersForNamespaces,
		assets.ReadFile,
		[]string{
			"rbac/privileged_role.yaml",
			"rbac/node_privileged_binding.yaml",
			"rbac/prometheus_role.yaml",
			"rbac/prometheus_rolebinding.yaml",
			"rbac/kube_rbac_proxy_role.yaml",
			"rbac/kube_rbac_proxy_binding.yaml",
			"rbac/initcontainer_role.yaml",
			"rbac/initcontainer_rolebinding.yaml",
			"rbac/lease_leader_election_role.yaml",
			"rbac/lease_leader_election_rolebinding.yaml",
			"rbac/main_attacher_binding.yaml",
			"rbac/main_provisioner_binding.yaml",
			"rbac/volumesnapshot_reader_provisioner_binding.yaml",
			"rbac/configmap_and_secret_reader_provisioner_binding.yaml",
			"rbac/main_resizer_binding.yaml",
			"rbac/main_snapshotter_binding.yaml",
			"configmap.yaml",
			"csidriver.yaml",
			"service.yaml",
			"cabundle_cm.yaml",
			"controller_sa.yaml",
			"node_sa.yaml",
			"network-policy-allow-ingress-to-csi-driver-metrics.yaml",
		},
	).WithConditionalStaticResourcesController(
		"IBMBlockDriverConditionalStaticResourcesController",
		kubeClient,
		dynamicClient,
		kubeInformersForNamespaces,
		assets.ReadFile,
		[]string{
			"volumesnapshotclass.yaml",
		},
		// Only install when CRD exists.
		func() bool {
			name := "volumesnapshotclasses.snapshot.storage.k8s.io"
			_, err := apiExtClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), name, metav1.GetOptions{})
			return err == nil
		},
		// Don't ever remove.
		func() bool {
			return false
		},
	).WithCSIConfigObserverController(
		"IBMBlockDriverCSIConfigObserverController",
		configInformers,
	).WithCSIDriverControllerService(
		"IBMBlockDriverControllerServiceController",
		assets.ReadFile,
		"controller.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(util.OperatorNamespace),
		configInformers,
		[]factory.Informer{
			nodeInformer.Informer(),
			secretInformer.Informer(),
			configMapInformer.Informer(),
		},
		csidrivercontrollerservicecontroller.WithObservedProxyDeploymentHook(),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(util.OperatorNamespace, util.MetricsCertSecretName, secretInformer),
		csidrivercontrollerservicecontroller.WithCABundleDeploymentHook(
			util.OperatorNamespace,
			util.TrustedCAConfigMap,
			configMapInformer,
		),
	).WithCSIDriverNodeService(
		"IBMBlockDriverNodeServiceController",
		assets.ReadFile,
		"node.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(util.OperatorNamespace),
		[]factory.Informer{configMapInformer.Informer()},
		csidrivernodeservicecontroller.WithObservedProxyDaemonSetHook(),
		csidrivernodeservicecontroller.WithCABundleDaemonSetHook(
			util.OperatorNamespace,
			util.TrustedCAConfigMap,
			configMapInformer,
		),
	).WithStorageClassController(
		"IBMBlockStorageClassController",
		assets.ReadFile,
		[]string{
			"storageclass/vpc-block-10iopsTier-StorageClass.yaml",
			"storageclass/vpc-block-5iopsTier-StorageClass.yaml",
			"storageclass/vpc-block-custom-StorageClass.yaml",
		},
		kubeClient,
		kubeInformersForNamespaces.InformersFor(""),
		operatorInformers,
		getEncryptionKeyHook(operatorInformers.Operator().V1().ClusterCSIDrivers().Lister()),
	)

	if err != nil {
		return err
	}

	secretSyncController := secret.NewSecretSyncController(
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces,
		util.Resync,
		controllerConfig.EventRecorder)

	serviceMonitorController := staticresourcecontroller.NewStaticResourceController(
		"IBMBlockDriverServiceMonitorController",
		assets.ReadFile,
		[]string{"servicemonitor.yaml"},
		(&resourceapply.ClientHolder{}).WithDynamicClient(dynamicClient),
		operatorClient,
		controllerConfig.EventRecorder,
	).WithIgnoreNotFoundOnCreate()

	klog.Info("Starting ServiceMonitor controller")
	go serviceMonitorController.Run(ctx, 1)

	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())
	go configInformers.Start(ctx.Done())
	go operatorInformers.Start(ctx.Done())

	klog.Info("Starting controllerset")
	go secretSyncController.Run(ctx, 1)
	go csiControllerSet.Run(ctx, 1)

	<-ctx.Done()

	return nil
}

func extractOperatorSpec(obj *unstructured.Unstructured, fieldManager string) (*applyopv1.OperatorSpecApplyConfiguration, error) {
	castObj := &opv1.ClusterCSIDriver{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, castObj); err != nil {
		return nil, fmt.Errorf("unable to convert to ClusterCSIDriver: %w", err)
	}
	ret, err := applyopv1.ExtractClusterCSIDriver(castObj, fieldManager)
	if err != nil {
		return nil, fmt.Errorf("unable to extract fields for %q: %w", fieldManager, err)
	}
	if ret.Spec == nil {
		return nil, nil
	}
	return &ret.Spec.OperatorSpecApplyConfiguration, nil
}

func extractOperatorStatus(obj *unstructured.Unstructured, fieldManager string) (*applyopv1.OperatorStatusApplyConfiguration, error) {
	castObj := &opv1.ClusterCSIDriver{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, castObj); err != nil {
		return nil, fmt.Errorf("unable to convert to ClusterCSIDriver: %w", err)
	}
	ret, err := applyopv1.ExtractClusterCSIDriverStatus(castObj, fieldManager)
	if err != nil {
		return nil, fmt.Errorf("unable to extract fields for %q: %w", fieldManager, err)
	}

	if ret.Status == nil {
		return nil, nil
	}
	return &ret.Status.OperatorStatusApplyConfiguration, nil
}
