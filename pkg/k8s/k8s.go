package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	containerimage "github.com/google/go-containerregistry/pkg/name"
	ms "github.com/mitchellh/mapstructure"
	"github.com/opencontainers/go-digest"
	corev1 "k8s.io/api/core/v1"
	k8sapierror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/strings/slices"

	"github.com/aquasecurity/trivy-kubernetes/pkg/bom"
	"github.com/aquasecurity/trivy-kubernetes/pkg/k8s/docker"
	"github.com/aquasecurity/trivy-kubernetes/utils"
)

var (
	UpstreamOrgName = map[string]string{
		"k8s.io":      "controller-manager,kubelet,apiserver,kubectl,kubernetes,kube-scheduler,kube-proxy,cloud-provider,ingress-nginx",
		"sigs.k8s.io": "secrets-store-csi-driver",
		"go.etcd.io":  "etcd/v3",
	}

	UpstreamRepoName = map[string]string{
		"kube-controller-manager":  "controller-manager",
		"kubelet":                  "kubelet",
		"kube-apiserver":           "apiserver",
		"kubectl":                  "kubectl",
		"kubernetes":               "kubernetes",
		"kube-scheduler":           "kube-scheduler",
		"kube-proxy":               "kube-proxy",
		"api server":               "apiserver",
		"etcd":                     "etcd/v3",
		"cloud-controller-manager": "cloud-provider",
		"secrets-store-csi-driver": "secrets-store-csi-driver",
	}
	CoreComponentPropertyType = map[string]string{
		"controller-manager": "controlPlane",
		"apiserver":          "controlPlane",
		"kube-scheduler":     "controlPlane",
		"etcd/v3":            "controlPlane",
		"cloud-provider":     "controlPlane",
		"kube-proxy":         "node",
	}
)

const (
	KindPod                   = "Pod"
	KindJob                   = "Job"
	KindCronJob               = "CronJob"
	KindReplicaSet            = "ReplicaSet"
	KindReplicationController = "ReplicationController"
	KindStatefulSet           = "StatefulSet"
	KindDaemonSet             = "DaemonSet"
	KindDeployment            = "Deployment"

	Deployments            = "deployments"
	ReplicaSets            = "replicasets"
	ReplicationControllers = "replicationcontrollers"
	StatefulSets           = "statefulsets"
	DaemonSets             = "daemonsets"
	CronJobs               = "cronjobs"
	Services               = "services"
	ServiceAccounts        = "serviceaccounts"
	Jobs                   = "jobs"
	Pods                   = "pods"
	ConfigMaps             = "configmaps"
	Roles                  = "roles"
	RoleBindings           = "rolebindings"
	NetworkPolicies        = "networkpolicies"
	Ingresses              = "ingresses"
	ResourceQuotas         = "resourcequotas"
	LimitRanges            = "limitranges"
	ClusterRoles           = "clusterroles"
	ClusterRoleBindings    = "clusterrolebindings"
	Nodes                  = "nodes"
	k8sComponentNamespace  = "kube-system"
	serviceAccountDefault  = "default"

	native   = "k8s"
	gke      = "gke"
	aks      = "aks"
	eks      = "eks"
	rke2     = "rke2"
	k3s      = "k3s"
	ocp      = "ocp"
	microk8s = "microk8s"
)

// Cluster interface represents the operations needed to scan a cluster
type Cluster interface {
	// GetCurrentContext returns local kubernetes current-context
	GetCurrentContext() string
	// GetCurrentNamespace returns local kubernetes current namespace
	GetCurrentNamespace() string
	// GetDynamicClient returns a dynamic k8s client
	GetDynamicClient() dynamic.Interface
	// GetK8sClientSet returns a k8s client set
	GetK8sClientSet() *kubernetes.Clientset
	// GetGVRs returns cluster GroupVersionResource to query kubernetes, receives
	// a boolean to determine if returns namespaced GVRs only or all GVRs, unless
	// resources is passed to filter
	GetGVRs(bool, []string) ([]schema.GroupVersionResource, error)
	// GetGVR returns resource GroupVersionResource to query kubernetes, receives
	// a string with the resource kind
	GetGVR(string) (schema.GroupVersionResource, error)
	// CreateBomComponents returns a list of BOM components by a namespace
	CreateBomComponents(ctx context.Context, namespace string) ([]bom.Component, error)
	// CreateClusterBom returns KBOM for a cluster
	CreateClusterBom(ctx context.Context) (*bom.Result, error)
	// GetClusterVersion return cluster git version
	GetClusterVersion() string
	// AuthByResource return image pull secrets by resource pod spec
	AuthByResource(resource unstructured.Unstructured) (map[string]docker.Auth, error)
	// SpecByPlatform return spec by platform type and version
	Platform() Platform
}

type cluster struct {
	currentContext   string
	currentNamespace string
	serverVersion    string
	dynamicClient    dynamic.Interface
	restMapper       meta.RESTMapper
	clientset        *kubernetes.Clientset
	cConfig          clientcmd.ClientConfig
}

type ClusterOption func(*genericclioptions.ConfigFlags)

// Specify the context to use, if empty uses default
func WithContext(context string) ClusterOption {
	return func(c *genericclioptions.ConfigFlags) {
		c.Context = &context
	}
}

// kubeconfig can be used to specify the config file path (overrides KUBECONFIG env)
func WithKubeConfig(kubeConfig string) ClusterOption {
	return func(c *genericclioptions.ConfigFlags) {
		c.KubeConfig = &kubeConfig
	}
}
func WithQPS(qps float32) ClusterOption {
	return func(o *genericclioptions.ConfigFlags) {
		o.WrapConfigFn = combineConfigFns(o.WrapConfigFn, func(c *rest.Config) *rest.Config {
			c.QPS = qps
			return c
		})
	}
}

func WithBurst(burst int) ClusterOption {
	return func(o *genericclioptions.ConfigFlags) {
		o.WrapConfigFn = combineConfigFns(o.WrapConfigFn, func(c *rest.Config) *rest.Config {
			c.Burst = burst
			return c
		})
	}
}

// Helper function to combine multiple config functions
func combineConfigFns(existing, newFn func(*rest.Config) *rest.Config) func(*rest.Config) *rest.Config {
	if existing == nil {
		return newFn
	}
	return func(c *rest.Config) *rest.Config {
		if modified := existing(c); modified != nil {
			return newFn(modified)
		}
		return newFn(c)
	}
}

// GetCluster returns a current configured cluster,
func GetCluster(opts ...ClusterOption) (Cluster, error) {
	cf := genericclioptions.NewConfigFlags(true)
	for _, opt := range opts {
		opt(cf)
	}

	// disable warnings
	rest.SetDefaultWarningHandler(rest.NoWarnings{})

	clientConfig := cf.ToRawKubeConfigLoader()

	restMapper, err := cf.ToRESTMapper()
	if err != nil {
		return nil, err
	}

	kubeConfig, err := cf.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	return getCluster(clientConfig, kubeConfig, restMapper, *cf.Context, false)
}

func getCluster(clientConfig clientcmd.ClientConfig, kubeConfig *rest.Config, restMapper meta.RESTMapper, currentContext string, fakeConfig bool) (*cluster, error) {

	k8sDynamicClient, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}
	var kubeClientset *kubernetes.Clientset

	kubeClientset, err = kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}
	rawCfg, err := clientConfig.RawConfig()
	if err != nil {
		return nil, err
	}

	var namespace string

	if len(currentContext) == 0 {
		currentContext = rawCfg.CurrentContext
	}
	if context, ok := rawCfg.Contexts[currentContext]; ok {
		namespace = context.Namespace
	}

	if len(namespace) == 0 {
		namespace = "default"
	}
	var serverVersion string
	if !fakeConfig {
		sv, err := kubeClientset.ServerVersion()
		if err != nil {
			return nil, err
		}
		serverVersion = strings.TrimPrefix(sv.GitVersion, "v")
	}
	return &cluster{
		currentContext:   currentContext,
		currentNamespace: namespace,
		dynamicClient:    k8sDynamicClient,
		restMapper:       restMapper,
		clientset:        kubeClientset,
		cConfig:          clientConfig,
		serverVersion:    serverVersion,
	}, nil
}

// GetCurrentContext returns local kubernetes current-context
func (c *cluster) GetCurrentContext() string {
	return c.currentContext
}

// GetClusterVersion return cluster git version
func (c *cluster) GetClusterVersion() string {
	return c.serverVersion
}

// GetCurrentNamespace returns local kubernetes current namespace
func (c *cluster) GetCurrentNamespace() string {
	return c.currentNamespace
}

// GetDynamicClient returns a dynamic k8s client
func (c *cluster) GetDynamicClient() dynamic.Interface {
	return c.dynamicClient
}

// GetK8sClientSet returns k8s clientSet
func (c *cluster) GetK8sClientSet() *kubernetes.Clientset {
	return c.clientset
}

// GetK8sClientSet returns k8s clientSet
func (c *cluster) Platform() Platform {
	platform, err := c.Platfrom()
	if err != nil {
		return Platform{Name: "k8s", Version: "1.23.0"}
	}
	return platform
}

func (cluster *cluster) Platfrom() (Platform, error) {
	v := cluster.getOpenShiftVersion(context.Background())
	if len(v) != 0 {
		return Platform{Name: "ocp", Version: majorVersion(v)}, nil
	}
	nodeName := cluster.getNodeName()
	semVersion, err := cluster.clientset.ServerVersion()
	if err != nil {
		return Platform{}, err
	}
	p := getPlatformInfoFromVersion(semVersion.GitVersion)
	var name string
	switch {
	case strings.Contains(p.Version, k3s):
		name = k3s
	case strings.Contains(p.Version, rke2):
		name = rke2
	case strings.Contains(p.Version, microk8s):
		name = microk8s
	case strings.Contains(nodeName, aks):
		name = aks
	case strings.Contains(nodeName, eks):
		name = eks
	case strings.Contains(nodeName, gke):
		name = gke
	default:
		name = "k8s"
	}
	return Platform{Name: name, Version: p.Version}, nil
}

type Platform struct {
	Name    string
	Version string
}

func (cluster *cluster) getOpenShiftVersion(ctx context.Context) string {
	gvr, err := cluster.restMapper.ResourceFor(schema.GroupVersionResource{Resource: "clusterversions"})
	if err != nil {
		return ""
	}
	dclient := cluster.dynamicClient.Resource(gvr).Namespace("")
	resources, err := dclient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	var version string
	for _, resource := range resources.Items {
		version, _, _ = unstructured.NestedString(resource.Object, []string{"status", "desired", "version"}...)

	}
	return version
}

func (cluster *cluster) getNodeName() string {
	nodes, err := cluster.clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return "k8s"
	}
	return nodes.Items[0].Name
}

// GetGVRs returns cluster GroupVersionResource to query kubernetes, receives
// a boolean to determine if returns namespaced GVRs only or all GVRs, unless
// resources is passed to filter
func (c *cluster) GetGVRs(namespaced bool, resources []string) ([]schema.GroupVersionResource, error) {
	grvs := make([]schema.GroupVersionResource, 0)
	if len(resources) == 0 {
		resources = getNamespaceResources()
		if !namespaced {
			resources = append(resources, getClusterResources()...)
		}
	}
	for _, resource := range resources {
		gvr, err := c.GetGVR(resource)
		if err != nil {
			return nil, err
		}

		grvs = append(grvs, gvr)
	}

	return grvs, nil
}

func (c *cluster) GetGVR(kind string) (schema.GroupVersionResource, error) {
	return c.restMapper.ResourceFor(schema.GroupVersionResource{Resource: kind})
}

// IsClusterResource returns if a GVR is a cluster resource
func IsClusterResource(gvr schema.GroupVersionResource) bool {
	for _, r := range getClusterResources() {
		if gvr.Resource == r {
			return true
		}
	}
	return false
}

// IsBuiltInWorkload returns true if the specified v1.OwnerReference
// is a built-in Kubernetes workload, false otherwise.
func IsBuiltInWorkload(resource *metav1.OwnerReference) bool {
	return resource != nil &&
		(resource.Kind == string(KindReplicaSet) ||
			resource.Kind == string(KindReplicationController) ||
			resource.Kind == string(KindStatefulSet) ||
			resource.Kind == string(KindDeployment) ||
			resource.Kind == string(KindCronJob) ||
			resource.Kind == string(KindDaemonSet) ||
			resource.Kind == string(KindJob))
}

func GetAllResources() []string {
	return append(getClusterResources(), getNamespaceResources()...)
}

func getClusterResources() []string {
	return []string{
		ClusterRoles,
		ClusterRoleBindings,
		Nodes,
	}
}

func getNamespaceResources() []string {
	return []string{
		Deployments,
		Pods,
		ReplicaSets,
		ReplicationControllers,
		StatefulSets,
		DaemonSets,
		CronJobs,
		Jobs,
		Services,
		ServiceAccounts,
		ConfigMaps,
		Roles,
		RoleBindings,
		NetworkPolicies,
		Ingresses,
		ResourceQuotas,
		LimitRanges,
	}
}

func (c *cluster) CreateBomComponents(ctx context.Context, namespace string) ([]bom.Component, error) {
	// collect addons info
	var components []bom.Component
	labels := map[string]string{
		namespace: "component",
	}
	if namespace != "" && c.isOpenShift() {
		labels = map[string]string{
			"openshift-kube-apiserver":          "apiserver",
			"openshift-kube-controller-manager": "kube-controller-manager",
			"openshift-kube-scheduler":          "scheduler",
			"openshift-etcd":                    "etcd",
		}
	}
	components, err := c.collectComponents(ctx, labels)
	if err != nil {
		return nil, err
	}
	addonLabels := map[string]string{
		namespace: "app.kubernetes.io/component=controller",
	}
	if namespace == "" || namespace == k8sComponentNamespace {
		addonLabels[k8sComponentNamespace] = "k8s-app"
	}
	addons, err := c.collectComponents(ctx, addonLabels)
	if err != nil {
		return nil, err
	}
	components = append(components, addons...)
	return components, nil
}

func (c *cluster) CreateClusterBom(ctx context.Context) (*bom.Result, error) {
	components, err := c.CreateBomComponents(ctx, "")
	if err != nil {
		return nil, err
	}
	nodesInfo, err := c.CollectNodes(components)
	if err != nil {
		return nil, err
	}
	return c.getClusterBomInfo(components, nodesInfo)
}

func extractDigest(imageId string) string {
	if strings.Contains(imageId, "@") {
		imageId = strings.Split(imageId, "@")[1]
	}
	return strings.TrimPrefix(imageId, string(digest.Canonical)+":")

}

// GetContainer returns a container object based on `imageName` (pod.Spec.Containers.Name)
// and `imageId` (pod.Status.ContainerStatuses.ImageID).
func GetContainer(imageName, imageId string) (bom.Container, error) {
	if strings.Contains(imageName, "@") {
		imageName = strings.Split(imageName, "@")[0]
	}
	imageRef, err := utils.ParseReference(imageName)
	if err != nil {
		return bom.Container{}, fmt.Errorf("unable to parse image name %q: %v", imageName, err)
	}
	// parse imageId to get the digest
	imageDigest := extractDigest(imageId)
	// skip non sha256 digests
	if len(imageDigest) != digest.Canonical.Size()*2 {
		return bom.Container{}, fmt.Errorf("unable to parse digest %q for %q", imageId, imageName)
	}

	repoName := imageRef.Context().RepositoryStr()
	registryName := imageRef.Context().RegistryStr()

	// Trim default namespace
	// See https://docs.docker.com/docker-hub/official_repos
	if registryName == containerimage.DefaultRegistry {
		repoName = strings.TrimPrefix(repoName, "library/")
	}

	version := imageRef.Identifier()
	return bom.Container{
		Repository: repoName,
		Registry:   registryName,
		ID:         fmt.Sprintf("%s:%s", repoName, version),
		Digest:     imageDigest,
		Version:    version,
	}, nil
}

func (c *cluster) CollectNodes(components []bom.Component) ([]bom.NodeInfo, error) {
	nodes, err := c.clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		if k8sapierror.IsNotFound(err) || k8sapierror.IsForbidden(err) {
			slog.Error("Unable to list node resources", "error", err)
			return []bom.NodeInfo{}, nil
		}
		return nil, err
	}
	nodesInfo := make([]bom.NodeInfo, 0)
	for _, node := range nodes.Items {
		nf := NodeInfo(node)
		images := make([]string, 0)
		for _, image := range node.Status.Images {
			for _, c := range components {
				for _, co := range c.Containers {
					id := fmt.Sprintf("%s/%s:%s", co.Registry, co.Repository, co.Version)
					if slices.Contains(image.Names, id) {
						images = append(images, id)
					}
				}
			}
		}
		nf.Images = images
		nodesInfo = append(nodesInfo, nf)
	}
	return nodesInfo, nil
}

func NodeInfo(node corev1.Node) bom.NodeInfo {
	nodeRole := "worker"
	if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
		nodeRole = "master"
	}
	if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
		nodeRole = "master"
	}
	return bom.NodeInfo{
		NodeName:                node.Name,
		KubeletVersion:          trimString(k8sVersions(node.Status.NodeInfo.KubeletVersion), []string{"v", "V"}),
		ContainerRuntimeVersion: node.Status.NodeInfo.ContainerRuntimeVersion,
		OsImage:                 node.Status.NodeInfo.OSImage,
		Properties: map[string]string{
			"NodeRole":        nodeRole,
			"HostName":        node.Name,
			"KernelVersion":   node.Status.NodeInfo.KernelVersion,
			"OperatingSystem": node.Status.NodeInfo.OperatingSystem,
			"Architecture":    node.Status.NodeInfo.Architecture,
		},
	}
}

func getPodsInfo(ctx context.Context, clientset *kubernetes.Clientset, labelSelector string, namespace string) (*corev1.PodList, error) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	return pods, nil
}

func (c *cluster) collectComponents(ctx context.Context, labels map[string]string) ([]bom.Component, error) {
	components := make([]bom.Component, 0)
	for namespace, labelSelector := range labels {
		pods, err := getPodsInfo(ctx, c.clientset, labelSelector, namespace)
		if err != nil {
			continue
		}
		for _, pod := range pods.Items {
			pi, err := PodInfo(pod, labelSelector)
			if err != nil {
				continue
			}
			components = append(components, *pi)
		}
	}
	return components, nil
}

func getImageIDsByStatuses(pod corev1.Pod) []string {
	ids := make([]string, len(pod.Spec.Containers))
	if len(pod.Spec.Containers) == 1 && len(pod.Status.ContainerStatuses) == 1 {
		ids[0] = getImageID(pod.Status.ContainerStatuses[0].ImageID)
		return ids
	}

	statusMap := make(map[string]string)
	for _, status := range pod.Status.ContainerStatuses {
		statusMap[status.Image] = status.ImageID
	}

	for i, container := range pod.Spec.Containers {
		if id, ok := statusMap[container.Image]; ok {
			ids[i] = getImageID(id)
			continue
		}
		ids[i] = getImageID(container.Image)
	}

	return ids
}

func PodInfo(pod corev1.Pod, labelSelector string) (*bom.Component, error) {
	containers := make([]bom.Container, 0)

	ids := getImageIDsByStatuses(pod)

	for i, s := range pod.Spec.Containers {
		container, err := GetContainer(s.Image, ids[i])
		if err != nil {
			slog.Warn("Unable to parse container", "error", err)
			continue
		}
		containers = append(containers, container)
	}
	props := make(map[string]string)

	labels := pod.GetLabels()

	name, version := labels["app.kubernetes.io/name"], labels["app.kubernetes.io/version"]
	props["Name"] = pod.Name
	props["Type"] = labels["app.kubernetes.io/component"]

	if name == "" {
		componentValue := labels[labelSelector]
		name = upstreamRepoByName(componentValue)
		if val, ok := CoreComponentPropertyType[name]; ok {
			props["Type"] = val
		}
	}
	orgName := upstreamOrgByName(name)
	if len(orgName) > 0 {
		name = fmt.Sprintf("%s/%s", orgName, name)
	}

	if version == "" {
		version = trimString(findComponentVersion(containers, labels[labelSelector]), []string{"v", "V"})
	}

	return &bom.Component{
		Namespace:  pod.Namespace,
		Name:       name,
		Version:    version,
		Properties: props,
		Containers: containers,
	}, nil
}

func findComponentVersion(containers []bom.Container, name string) string {
	for _, c := range containers {
		if strings.Contains(c.Version, "rke2") || strings.Contains(c.Version, "k3s") {
			return k8sVersions(c.Version)
		} else if strings.Contains(c.ID, name) {
			return c.Version
		}
	}
	return ""
}

func k8sVersions(version string) string {
	switch {
	case strings.Contains(version, "+rke2"):
		index := strings.Index(version, "+rke2")
		return version[:index]
	case strings.Contains(version, "-rke2"):
		index := strings.Index(version, "-rke2")
		return version[:index]
	case strings.Contains(version, "-k3s"):
		index := strings.Index(version, "-k3s")
		return version[:index]
	}
	return version
}

func (c *cluster) isOpenShift() bool {
	ctx := context.Background()
	_, err := c.clientset.CoreV1().Namespaces().Get(ctx, "openshift-kube-apiserver", metav1.GetOptions{})
	return !k8sapierror.IsNotFound(err)
}

func (c *cluster) getClusterBomInfo(components []bom.Component, nodeInfo []bom.NodeInfo) (*bom.Result, error) {
	name, version, err := c.ClusterNameVersion()
	if err != nil {
		return nil, err
	}
	br := &bom.Result{
		Components: components,
		ID:         "k8s.io/kubernetes",
		Type:       "Cluster",
		Version:    trimString(version, []string{"v", "V"}),
		Properties: map[string]string{"Name": name, "Type": "cluster"},
		NodesInfo:  nodeInfo,
	}
	return br, nil
}

func (c *cluster) ClusterNameVersion() (string, string, error) {
	rawCfg, err := c.cConfig.RawConfig()
	if err != nil {
		return "", "", err
	}
	clusterName := "k8s.io/kubernetes"
	if len(rawCfg.Contexts) > 0 {
		if c.currentContext != "" {
			rawCfg.CurrentContext = c.currentContext
		}
		if clusterContext, ok := rawCfg.Contexts[rawCfg.CurrentContext]; ok {
			clusterName = clusterContext.Cluster
		}
	}
	version, err := c.clientset.ServerVersion()
	if err != nil {
		return "", "", err
	}
	return clusterName, version.GitVersion, nil
}

// ListImagePullSecretsByPodSpec return image pull secrets by pod spec
func (r *cluster) ListImagePullSecretsByPodSpec(ctx context.Context, spec *corev1.PodSpec, ns string) (map[string]docker.Auth, error) {
	if spec == nil {
		return map[string]docker.Auth{}, nil
	}
	imagePullSecrets := spec.ImagePullSecrets

	sa, err := r.getServiceAccountByPodSpec(ctx, spec, ns)
	if err != nil && !k8sapierror.IsNotFound(err) && !k8sapierror.IsForbidden(err) {
		return nil, err
	}
	imagePullSecrets = append(sa.ImagePullSecrets, imagePullSecrets...)

	secrets, err := r.ListByLocalObjectReferences(ctx, imagePullSecrets, ns)
	if err != nil {
		return nil, err
	}

	return mapDockerRegistryServersToAuths(secrets, true)
}

func (r *cluster) getServiceAccountByPodSpec(ctx context.Context, spec *corev1.PodSpec, ns string) (*corev1.ServiceAccount, error) {
	serviceAccountName := spec.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = serviceAccountDefault
	}
	sa, err := r.clientset.CoreV1().ServiceAccounts(ns).Get(ctx, serviceAccountName, metav1.GetOptions{})
	if err != nil {
		return sa, fmt.Errorf("getting service account by name: %s/%s: %w", ns, serviceAccountName, err)
	}
	return sa, nil
}

func (r *cluster) ListByLocalObjectReferences(ctx context.Context, refs []corev1.LocalObjectReference, ns string) ([]*corev1.Secret, error) {
	secrets := make([]*corev1.Secret, 0)

	for _, secretRef := range refs {
		if secretRef.Name == "" {
			continue
		}
		secret, err := r.clientset.CoreV1().Secrets(ns).Get(ctx, secretRef.Name, metav1.GetOptions{})
		if err != nil {
			if k8sapierror.IsNotFound(err) || k8sapierror.IsForbidden(err) {
				continue
			}
			return nil, fmt.Errorf("getting secret by name: %s/%s: %w", ns, secretRef.Name, err)
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

// MapDockerRegistryServersToAuths creates the mapping from a Docker registry server
// to the Docker authentication credentials for the specified slice of image pull Secrets.
func mapDockerRegistryServersToAuths(imagePullSecrets []*corev1.Secret, multiSecretSupport bool) (map[string]docker.Auth, error) {
	auths := make(map[string]docker.Auth)
	for _, secret := range imagePullSecrets {
		var data []byte
		var hasRequiredData, isLegacy bool

		switch secret.Type {
		case corev1.SecretTypeDockerConfigJson:
			data, hasRequiredData = secret.Data[corev1.DockerConfigJsonKey]
		case corev1.SecretTypeDockercfg:
			data, hasRequiredData = secret.Data[corev1.DockerConfigKey]
			isLegacy = true
		default:
			continue
		}

		// Skip a secrets of type "kubernetes.io/dockerconfigjson" or "kubernetes.io/dockercfg" which does not contain
		// the required ".dockerconfigjson" or ".dockercfg" key.
		if !hasRequiredData {
			continue
		}
		dockerConfig := &docker.Config{}
		err := dockerConfig.Read(data, isLegacy)
		if err != nil {
			return nil, fmt.Errorf("reading %s or %s field of %q secret: %w", corev1.DockerConfigJsonKey, corev1.DockerConfigKey, secret.Namespace+"/"+secret.Name, err)
		}
		for authKey, auth := range dockerConfig.Auths {
			server, err := docker.GetServerFromDockerAuthKey(authKey)
			if err != nil {
				return nil, err
			}
			if a, ok := auths[server]; multiSecretSupport && ok {
				user := fmt.Sprintf("%s,%s", a.Username, auth.Username)
				pass := fmt.Sprintf("%s,%s", a.Password, auth.Password)
				auths[server] = docker.Auth{Username: user, Password: pass}
			} else {
				auths[server] = auth
			}
		}
	}
	return auths, nil
}

type ContainerImages map[string]string

func MapContainerNamesToDockerAuths(imageRef string, auths map[string]docker.Auth) (*docker.Auth, error) {
	wildcardServers := GetWildcardServers(auths)

	var authsCred docker.Auth
	server, err := docker.GetServerFromImageRef(imageRef)
	if err != nil {
		return &authsCred, err
	}
	if auth, ok := auths[server]; ok {
		return &auth, nil
	}
	if len(wildcardServers) > 0 {
		if wildcardDomain := matchSubDomain(wildcardServers, server); len(wildcardDomain) > 0 {
			if auth, ok := auths[wildcardDomain]; ok {
				return &auth, nil
			}
		}
	}

	return nil, nil
}

func GetWildcardServers(auths map[string]docker.Auth) []string {
	wildcardServers := make([]string, 0)
	for server := range auths {
		if strings.HasPrefix(server, "*.") {
			wildcardServers = append(wildcardServers, server)
		}
	}
	return wildcardServers
}

func matchSubDomain(wildcardServers []string, subDomain string) string {
	for _, domain := range wildcardServers {
		domainWithoutWildcard := strings.Replace(domain, "*", "", 1)
		if strings.HasSuffix(subDomain, domainWithoutWildcard) {
			return domain
		}
	}
	return ""
}

func getWorkloadPodSpec(un unstructured.Unstructured) (*corev1.PodSpec, error) {
	switch un.GetKind() {
	case KindPod:
		objectMap, ok, err := unstructured.NestedMap(un.Object, []string{"spec"}...)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("unstructured resource do not match Pod spec")
		}
		return mapToPodSpec(objectMap)
	case KindCronJob:
		objectMap, ok, err := unstructured.NestedMap(un.Object, []string{"spec", "jobTemplate", "spec", "template", "spec"}...)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("unstructured resource do not match Pod spec")
		}
		return mapToPodSpec(objectMap)
	case KindDeployment, KindReplicaSet, KindReplicationController, KindStatefulSet, KindDaemonSet, KindJob:
		objectMap, ok, err := unstructured.NestedMap(un.Object, []string{"spec", "template", "spec"}...)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("unstructured resource do not match Pod spec")
		}
		return mapToPodSpec(objectMap)
	default:
		return nil, nil
	}
}

func mapToPodSpec(objectMap map[string]interface{}) (*corev1.PodSpec, error) {
	ps := &corev1.PodSpec{}
	err := ms.Decode(objectMap, ps)
	if err != nil && len(ps.Containers) == 0 {
		return nil, err
	}
	return ps, nil
}

func (r *cluster) AuthByResource(resource unstructured.Unstructured) (map[string]docker.Auth, error) {
	podSpec, err := getWorkloadPodSpec(resource)
	if err != nil {
		return nil, err
	}
	var serverAuths map[string]docker.Auth
	serverAuths, err = r.ListImagePullSecretsByPodSpec(context.Background(), podSpec, resource.GetNamespace())
	if err != nil {
		return nil, err
	}
	return serverAuths, nil
}

func upstreamOrgByName(component string) string {
	for key, components := range UpstreamOrgName {
		for _, c := range strings.Split(components, ",") {
			if strings.TrimSpace(c) == strings.ToLower(component) {
				return key
			}
		}
	}
	return ""
}

func upstreamRepoByName(component string) string {
	if val, ok := UpstreamRepoName[component]; ok {
		return val
	}

	return component
}

func trimString(version string, trimValues []string) string {
	for _, v := range trimValues {
		version = strings.Trim(version, v)
	}
	return strings.TrimSpace(version)
}

func getImageID(image string) string {
	if strings.HasPrefix(image, digest.Canonical.String()+":") {
		return image
	}
	imageParts := strings.Split(image, "@")
	if len(imageParts) > 1 && strings.HasPrefix(imageParts[1], digest.Canonical.String()+":") {
		return imageParts[1]
	}
	return ""
}
