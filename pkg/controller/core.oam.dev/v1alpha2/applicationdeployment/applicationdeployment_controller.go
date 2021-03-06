package applicationdeployment

import (
	"context"
	"fmt"
	"time"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/util/slice"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oamv1alpha2 "github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha2"
	"github.com/oam-dev/kubevela/pkg/controller/common/rollout"
	controller "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
)

const appDeployFinalizer = "finalizers.applicationdeployment.oam.dev"
const reconcileTimeOut = 30 * time.Second

// Reconciler reconciles an ApplicationDeployment object
type Reconciler struct {
	client.Client
	dm     discoverymapper.DiscoveryMapper
	record event.Recorder
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.oam.dev,resources=applicationdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.oam.dev,resources=applicationdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.oam.dev,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.oam.dev,resources=applications/status,verbs=get;update;patch

// Reconcile is the main logic of applicationdeployment controller
func (r *Reconciler) Reconcile(req ctrl.Request) (res reconcile.Result, retErr error) {
	var appDeploy oamv1alpha2.ApplicationDeployment
	ctx, cancel := context.WithTimeout(context.TODO(), reconcileTimeOut)
	defer cancel()

	startTime := time.Now()
	defer func() {
		if retErr == nil {
			if res.Requeue || res.RequeueAfter > 0 {
				klog.InfoS("Finished reconciling appDeployment", "deployment", req, "time spent",
					time.Since(startTime), "result", res)
			} else {
				klog.InfoS("Finished reconcile appDeployment", "deployment", req, "time spent", time.Since(startTime))
			}
		} else {
			klog.Errorf("Failed to reconcile appDeployment %s: %v", req, retErr)
		}
	}()

	if err := r.Get(ctx, req.NamespacedName, &appDeploy); err != nil {
		if apierrors.IsNotFound(err) {
			klog.InfoS("application deployment does not exist", "appDeploy", klog.KRef(req.Namespace, req.Name))
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	klog.InfoS("Start to reconcile ", "application deployment", klog.KObj(&appDeploy))

	// TODO: check if the target/source has changed
	r.handleFinalizer(&appDeploy)

	// Get the target application
	var targetApp oamv1alpha2.ApplicationConfiguration
	var sourceApp *oamv1alpha2.ApplicationConfiguration
	targetAppName := appDeploy.Spec.TargetApplicationName
	if err := r.Get(ctx, ktypes.NamespacedName{Namespace: req.Namespace, Name: targetAppName},
		&targetApp); err != nil {
		klog.ErrorS(err, "cannot locate target application", "target application",
			klog.KRef(req.Namespace, targetAppName))
		return ctrl.Result{}, err
	}

	// Get the source application
	sourceAppName := appDeploy.Spec.SourceApplicationName
	if sourceAppName == "" {
		klog.Info("source app fields not filled, we assume it is deployed for the first time")
	} else if err := r.Get(ctx, ktypes.NamespacedName{Namespace: req.Namespace, Name: sourceAppName}, sourceApp); err != nil {
		klog.ErrorS(err, "cannot locate source application", "source application", klog.KRef(req.Namespace,
			sourceAppName))
		return ctrl.Result{}, err
	}

	targetWorkload, sourceWorkload, err := r.extractWorkloads(ctx, appDeploy.Spec.ComponentList, &targetApp, sourceApp)
	if err != nil {
		klog.ErrorS(err, "cannot fetch the workloads to upgrade", "target application",
			klog.KRef(req.Namespace, targetAppName), "source application", klog.KRef(req.Namespace, sourceAppName),
			"commonComponent", appDeploy.Spec.ComponentList)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, client.IgnoreNotFound(err)
	}
	klog.InfoS("get the target workload we need to work on", "targetWorkload", klog.KObj(targetWorkload))

	if sourceWorkload != nil {
		klog.InfoS("get the source workload we need to work on", "sourceWorkload", klog.KObj(sourceWorkload))
	}

	// reconcile the rollout part of the spec given the target and source workload
	rolloutPlanController := rollout.NewRolloutPlanController(r, &appDeploy, r.record,
		&appDeploy.Spec.RolloutPlan, &appDeploy.Status.RolloutStatus, targetWorkload, sourceWorkload)
	result, rolloutStatus := rolloutPlanController.Reconcile(ctx)
	// make sure that the new status is copied back
	appDeploy.Status.RolloutStatus = *rolloutStatus
	// update the appDeploy status
	return result, r.Update(ctx, &appDeploy)
}

func (r *Reconciler) handleFinalizer(appDeploy *oamv1alpha2.ApplicationDeployment) {
	if appDeploy.DeletionTimestamp.IsZero() {
		if !slice.ContainsString(appDeploy.Finalizers, appDeployFinalizer, nil) {
			// TODO: add finalizer
			klog.Info("add finalizer")
		}
	} else if slice.ContainsString(appDeploy.Finalizers, appDeployFinalizer, nil) {
		// TODO: perform finalize
		klog.Info("perform clean up")
	}
}

// SetupWithManager setup the controller with manager
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.record = event.NewAPIRecorder(mgr.GetEventRecorderFor("ApplicationDeployment")).
		WithAnnotations("controller", "ApplicationDeployment")
	return ctrl.NewControllerManagedBy(mgr).
		For(&oamv1alpha2.ApplicationDeployment{}).
		Owns(&oamv1alpha2.Application{}).
		Complete(r)
}

// Setup adds a controller that reconciles ApplicationDeployment.
func Setup(mgr ctrl.Manager, _ controller.Args, _ logging.Logger) error {
	dm, err := discoverymapper.New(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("create discovery dm fail %w", err)
	}
	reconciler := Reconciler{
		Client: mgr.GetClient(),
		dm:     dm,
		Scheme: mgr.GetScheme(),
	}
	return reconciler.SetupWithManager(mgr)
}
