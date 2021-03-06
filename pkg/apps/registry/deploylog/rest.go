package deploylog

import (
	"fmt"
	"sort"
	"time"

	"github.com/golang/glog"
	kapiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericrest "k8s.io/apiserver/pkg/registry/generic/rest"
	"k8s.io/apiserver/pkg/registry/rest"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	kcoreclient "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/core/internalversion"
	"k8s.io/kubernetes/pkg/controller"
	kcmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"

	apiserverrest "github.com/openshift/origin/pkg/apiserver/rest"
	appsapi "github.com/openshift/origin/pkg/apps/apis/apps"
	"github.com/openshift/origin/pkg/apps/apis/apps/validation"
	appsclient "github.com/openshift/origin/pkg/apps/generated/internalclientset/typed/apps/internalversion"
	"github.com/openshift/origin/pkg/apps/registry"
	appsutil "github.com/openshift/origin/pkg/apps/util"
)

const (
	// defaultTimeout is the default time to wait for the logs of a deployment.
	defaultTimeout = 60 * time.Second
	// defaultInterval is the default interval for polling a not found deployment.
	defaultInterval = 1 * time.Second
)

// REST is an implementation of RESTStorage for the api server.
type REST struct {
	dcClient  appsclient.DeploymentConfigsGetter
	rcClient  kcoreclient.ReplicationControllersGetter
	podClient kcoreclient.PodsGetter
	timeout   time.Duration
	interval  time.Duration

	// for unit testing
	getLogsFn func(podNamespace, podName string, logOpts *kapi.PodLogOptions) (runtime.Object, error)
}

// REST implements GetterWithOptions
var _ = rest.GetterWithOptions(&REST{})

// NewREST creates a new REST for DeploymentLogs. It uses three clients: one for configs,
// one for deployments (replication controllers) and one for pods to get the necessary
// attributes to assemble the URL to which the request shall be redirected in order to
// get the deployment logs.
func NewREST(dcClient appsclient.DeploymentConfigsGetter, rcClient kcoreclient.ReplicationControllersGetter, podClient kcoreclient.PodsGetter) *REST {
	r := &REST{
		dcClient:  dcClient,
		rcClient:  rcClient,
		podClient: podClient,
		timeout:   defaultTimeout,
		interval:  defaultInterval,
	}
	r.getLogsFn = r.getLogs

	return r
}

// NewGetOptions returns a new options object for deployment logs
func (r *REST) NewGetOptions() (runtime.Object, bool, string) {
	return &appsapi.DeploymentLogOptions{}, false, ""
}

// New creates an empty DeploymentLog resource
func (r *REST) New() runtime.Object {
	return &appsapi.DeploymentLog{}
}

// Get returns a streamer resource with the contents of the deployment log
func (r *REST) Get(ctx apirequest.Context, name string, opts runtime.Object) (runtime.Object, error) {
	// Ensure we have a namespace in the context
	namespace, ok := apirequest.NamespaceFrom(ctx)
	if !ok {
		return nil, errors.NewBadRequest("namespace parameter required.")
	}

	// Validate DeploymentLogOptions
	deployLogOpts, ok := opts.(*appsapi.DeploymentLogOptions)
	if !ok {
		return nil, errors.NewBadRequest("did not get an expected options.")
	}
	if errs := validation.ValidateDeploymentLogOptions(deployLogOpts); len(errs) > 0 {
		return nil, errors.NewInvalid(appsapi.Kind("DeploymentLogOptions"), "", errs)
	}

	// Fetch deploymentConfig and check latest version; if 0, there are no deployments
	// for this config
	config, err := r.dcClient.DeploymentConfigs(namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.NewNotFound(appsapi.Resource("deploymentconfig"), name)
	}
	desiredVersion := config.Status.LatestVersion
	if desiredVersion == 0 {
		return nil, errors.NewBadRequest(fmt.Sprintf("no deployment exists for deploymentConfig %q", config.Name))
	}

	// Support retrieving logs for older deployments
	switch {
	case deployLogOpts.Version == nil:
		// Latest or previous
		if deployLogOpts.Previous {
			desiredVersion--
			if desiredVersion < 1 {
				return nil, errors.NewBadRequest(fmt.Sprintf("no previous deployment exists for deploymentConfig %q", config.Name))
			}
		}
	case *deployLogOpts.Version <= 0 || *deployLogOpts.Version > config.Status.LatestVersion:
		// Invalid version
		return nil, errors.NewBadRequest(fmt.Sprintf("invalid version for deploymentConfig %q: %d", config.Name, *deployLogOpts.Version))
	default:
		desiredVersion = *deployLogOpts.Version
	}

	// Get desired deployment
	targetName := appsutil.DeploymentNameForConfigVersion(config.Name, desiredVersion)
	target, err := r.waitForExistingDeployment(namespace, targetName)
	if err != nil {
		return nil, err
	}
	podName := appsutil.DeployerPodNameForDeployment(target.Name)

	// Check for deployment status; if it is new or pending, we will wait for it. If it is complete,
	// the deployment completed successfully and the deployer pod will be deleted so we will return a
	// success message. If it is running or failed, retrieve the log from the deployer pod.
	status := appsutil.DeploymentStatusFor(target)
	switch status {
	case appsapi.DeploymentStatusNew, appsapi.DeploymentStatusPending:
		if deployLogOpts.NoWait {
			glog.V(4).Infof("Deployment %s is in %s state. No logs to retrieve yet.", appsutil.LabelForDeployment(target), status)
			return &genericrest.LocationStreamer{}, nil
		}
		glog.V(4).Infof("Deployment %s is in %s state, waiting for it to start...", appsutil.LabelForDeployment(target), status)

		if err := appsutil.WaitForRunningDeployerPod(r.podClient, target, r.timeout); err != nil {
			return nil, errors.NewBadRequest(fmt.Sprintf("failed to run deployer pod %s: %v", podName, err))
		}

		latest, ok, err := registry.WaitForRunningDeployment(r.rcClient, target, r.timeout)
		if err != nil {
			return nil, errors.NewBadRequest(fmt.Sprintf("unable to wait for deployment %s to run: %v", appsutil.LabelForDeployment(target), err))
		}
		if !ok {
			return nil, errors.NewServerTimeout(kapi.Resource("ReplicationController"), "get", 2)
		}
		if appsutil.IsCompleteDeployment(latest) {
			podName, err = r.returnApplicationPodName(target)
			if err != nil {
				return nil, err
			}
		}
	case appsapi.DeploymentStatusComplete:
		podName, err = r.returnApplicationPodName(target)
		if err != nil {
			return nil, err
		}
	}

	logOpts := appsapi.DeploymentToPodLogOptions(deployLogOpts)
	return r.getLogsFn(namespace, podName, logOpts)
}

func (r *REST) getLogs(podNamespace, podName string, logOpts *kapi.PodLogOptions) (runtime.Object, error) {
	logRequest := r.podClient.Pods(podNamespace).GetLogs(podName, logOpts)

	readerCloser, err := logRequest.Stream()
	if err != nil {
		return nil, err
	}

	return &apiserverrest.PassThroughStreamer{
		In:          readerCloser,
		Flush:       logOpts.Follow,
		ContentType: "text/plain",
	}, nil
}

// waitForExistingDeployment will use the timeout to wait for a deployment to appear.
func (r *REST) waitForExistingDeployment(namespace, name string) (*kapi.ReplicationController, error) {
	var (
		target *kapi.ReplicationController
		err    error
	)

	condition := func() (bool, error) {
		target, err = r.rcClient.ReplicationControllers(namespace).Get(name, metav1.GetOptions{})
		switch {
		case errors.IsNotFound(err):
			return false, nil
		case err != nil:
			return false, err
		}
		return true, nil
	}

	err = wait.PollImmediate(r.interval, r.timeout, condition)
	if err == wait.ErrWaitTimeout {
		err = errors.NewNotFound(kapi.Resource("replicationcontrollers"), name)
	}
	return target, err
}

// returnApplicationPodName returns the best candidate pod for the target deployment in order to
// view its logs.
func (r *REST) returnApplicationPodName(target *kapi.ReplicationController) (string, error) {
	selector := labels.SelectorFromValidatedSet(labels.Set(target.Spec.Selector))
	sortBy := func(pods []*kapiv1.Pod) sort.Interface { return controller.ByLogging(pods) }

	firstPod, _, err := kcmdutil.GetFirstPod(r.podClient, target.Namespace, selector.String(), r.timeout, sortBy)
	if err != nil {
		return "", errors.NewInternalError(err)
	}
	return firstPod.Name, nil
}
