package liveupdate

import (
	"context"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/tilt-dev/tilt/internal/build"
	"github.com/tilt-dev/tilt/internal/container"
	"github.com/tilt-dev/tilt/internal/containerupdate"
	"github.com/tilt-dev/tilt/internal/controllers/apicmp"
	"github.com/tilt-dev/tilt/internal/controllers/apis/liveupdate"
	"github.com/tilt-dev/tilt/internal/controllers/indexer"
	"github.com/tilt-dev/tilt/internal/k8s"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/internal/store/liveupdates"
	"github.com/tilt-dev/tilt/pkg/apis"
	"github.com/tilt-dev/tilt/pkg/apis/core/v1alpha1"
	"github.com/tilt-dev/tilt/pkg/logger"
)

var discoveryGVK = v1alpha1.SchemeGroupVersion.WithKind("KubernetesDiscovery")
var applyGVK = v1alpha1.SchemeGroupVersion.WithKind("KubernetesApply")
var fwGVK = v1alpha1.SchemeGroupVersion.WithKind("FileWatch")
var imageMapGVK = v1alpha1.SchemeGroupVersion.WithKind("ImageMap")

// Manages the LiveUpdate API object.
type Reconciler struct {
	client  ctrlclient.Client
	indexer *indexer.Indexer
	store   store.RStore

	ExecUpdater   containerupdate.ContainerUpdater
	DockerUpdater containerupdate.ContainerUpdater
	updateMode    liveupdates.UpdateMode
	kubeContext   k8s.KubeContext
	startedTime   metav1.MicroTime

	monitors map[string]*monitor

	// TODO(nick): Remove this mutex once ForceApply is gone.
	mu sync.Mutex
}

var _ reconcile.Reconciler = &Reconciler{}

// Dependency-inject a live update reconciler.
func NewReconciler(
	st store.RStore,
	dcu *containerupdate.DockerUpdater,
	ecu *containerupdate.ExecUpdater,
	updateMode liveupdates.UpdateMode,
	kubeContext k8s.KubeContext,
	client ctrlclient.Client,
	scheme *runtime.Scheme) *Reconciler {
	return &Reconciler{
		DockerUpdater: dcu,
		ExecUpdater:   ecu,
		updateMode:    updateMode,
		kubeContext:   kubeContext,
		client:        client,
		indexer:       indexer.NewIndexer(scheme, indexLiveUpdate),
		store:         st,
		startedTime:   apis.NowMicro(),
		monitors:      make(map[string]*monitor),
	}
}

// Create a reconciler baked by a fake ContainerUpdater and Client.
func NewFakeReconciler(
	st store.RStore,
	cu containerupdate.ContainerUpdater,
	client ctrlclient.Client) *Reconciler {
	scheme := v1alpha1.NewScheme()
	return &Reconciler{
		DockerUpdater: cu,
		ExecUpdater:   cu,
		updateMode:    liveupdates.UpdateModeAuto,
		kubeContext:   k8s.KubeContext("fake-context"),
		client:        client,
		indexer:       indexer.NewIndexer(scheme, indexLiveUpdate),
		store:         st,
		startedTime:   apis.NowMicro(),
		monitors:      make(map[string]*monitor),
	}
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	lu := &v1alpha1.LiveUpdate{}
	err := r.client.Get(ctx, req.NamespacedName, lu)
	r.indexer.OnReconcile(req.NamespacedName, lu)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("liveupdate reconcile: %v", err)
	}

	if apierrors.IsNotFound(err) || lu.ObjectMeta.DeletionTimestamp != nil {
		r.store.Dispatch(liveupdates.NewLiveUpdateDeleteAction(req.Name))
		delete(r.monitors, req.Name)
		return ctrl.Result{}, nil
	}

	// The apiserver is the source of truth, and will ensure the engine state is up to date.
	r.store.Dispatch(liveupdates.NewLiveUpdateUpsertAction(lu))

	ctx = store.MustObjectLogHandler(ctx, r.store, lu)

	if lu.Annotations[v1alpha1.AnnotationManagedBy] != "" {
		// A LiveUpdate can't be managed by the reconciler until all the objects
		// it depends on are managed by the reconciler. The Tiltfile controller
		// is responsible for marking objects that we want to manage with ForceApply().
		return ctrl.Result{}, nil
	}

	monitor := r.ensureMonitorExists(lu.Name, lu.Spec)
	hasFileChanges, err := r.reconcileFileWatches(ctx, monitor)
	if err != nil {
		return ctrl.Result{}, err
	}

	hasKubernetesChanges, err := r.reconcileKubernetesResource(ctx, monitor)
	if err != nil {
		return ctrl.Result{}, err
	}

	if hasFileChanges || hasKubernetesChanges {
		monitor.hasChangesToSync = true
	}

	if monitor.hasChangesToSync {
		err := r.maybeSync(ctx, lu, monitor)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	monitor.hasChangesToSync = false

	return ctrl.Result{}, nil
}

// Create the monitor that tracks a live update. If the live update
// spec changes, wipe out all accumulated state.
func (r *Reconciler) ensureMonitorExists(name string, spec v1alpha1.LiveUpdateSpec) *monitor {
	m, ok := r.monitors[name]
	if ok && apicmp.DeepEqual(spec, m.spec) {
		return m
	}

	m = &monitor{
		spec:           spec,
		modTimeByPath:  make(map[string]metav1.MicroTime),
		lastFileEvents: make(map[string]*v1alpha1.FileEvent),
		containers:     make(map[monitorContainerKey]monitorContainerStatus),
	}
	r.monitors[name] = m
	return m
}

// Consume all FileEvents off the FileWatch objects.
// Returns true if we saw new file events.
//
// TODO(nick): Currently, it's entirely possible to miss file events.  This has
// always been true (since operating systems themselves put limits on the event
// queue.) But it gets worse in a world where we read FileEvents from the API,
// since the FileWatch API itself adds lower limits.
//
// Long-term, we ought to have some way to reconnect/resync like other
// sync systems do (syncthing/rsync). e.g., diff the two file systems
// and update based on changes. But it also might make more sense to switch to a
// different library for syncing (e.g., Mutagen) now that live updates
// are decoupled from other file event-triggered tasks.
//
// In the meantime, Milas+Nick should figure out a way to handle this
// better in the short term.
func (r *Reconciler) reconcileFileWatches(ctx context.Context, monitor *monitor) (bool, error) {
	if len(monitor.spec.FileWatchNames) == 0 {
		return false, nil
	}

	hasChange := false
	for _, fwn := range monitor.spec.FileWatchNames {
		oneChange, err := r.reconcileOneFileWatch(ctx, monitor, fwn)
		if err != nil {
			return false, err
		}
		if oneChange {
			hasChange = true
		}
	}
	return hasChange, nil
}

// Consume one FileWatch object.
func (r *Reconciler) reconcileOneFileWatch(ctx context.Context, monitor *monitor, fwn string) (bool, error) {
	var fw v1alpha1.FileWatch
	err := r.client.Get(ctx, types.NamespacedName{Name: fwn}, &fw)
	if err != nil {
		// Do nothing if an object hasn't appeared yet.
		//
		// TODO(nick): We should have some failure state for LiveUpdateStatus
		// for when an object it depends on isn't in the API server.
		// This may not be a permanent failure state, because
		// the object just hasn't been created yet.
		return false, client.IgnoreNotFound(err)
	}

	events := fw.Status.FileEvents
	if len(events) == 0 {
		return false, nil
	}

	newLastFileEvent := events[len(events)-1]
	event := monitor.lastFileEvents[fwn]
	if event != nil && apicmp.DeepEqual(&newLastFileEvent, event) {
		return false, nil
	}
	monitor.lastFileEvents[fwn] = &newLastFileEvent

	// Consume all the file events.
	for _, event := range events {
		for _, f := range event.SeenFiles {
			existing, ok := monitor.modTimeByPath[f]
			if !ok || existing.Time.Before(event.Time.Time) {
				monitor.modTimeByPath[f] = event.Time
			}
		}
	}
	return true, nil
}

// Consume all objects off the KubernetesSelector.
// Returns true if we saw any changes to the objects we're watching.
func (r *Reconciler) reconcileKubernetesResource(ctx context.Context, monitor *monitor) (bool, error) {
	selector := monitor.spec.Selector.Kubernetes
	if selector == nil {
		return false, nil
	}

	// TODO(nick): Update the status field with an error if there's no discovery name.

	var kd *v1alpha1.KubernetesDiscovery
	var ka *v1alpha1.KubernetesApply
	var im *v1alpha1.ImageMap
	changed := false
	if selector.ApplyName != "" {
		ka = &v1alpha1.KubernetesApply{}
		err := r.client.Get(ctx, types.NamespacedName{Name: selector.ApplyName}, ka)
		if err != nil {
			// Do nothing if an object hasn't appeared yet.
			return false, client.IgnoreNotFound(err)
		}

		if monitor.lastKubernetesApplyStatus == nil ||
			!apicmp.DeepEqual(monitor.lastKubernetesApplyStatus, &(ka.Status)) {
			changed = true
		}
	}

	if selector.DiscoveryName != "" {
		kd = &v1alpha1.KubernetesDiscovery{}
		err := r.client.Get(ctx, types.NamespacedName{Name: selector.DiscoveryName}, kd)
		if err != nil {
			// Do nothing if an object hasn't appeared yet.
			return false, client.IgnoreNotFound(err)
		}

		if monitor.lastKubernetesDiscovery == nil ||
			!apicmp.DeepEqual(monitor.lastKubernetesDiscovery.Status, kd.Status) {
			changed = true
		}
	}

	if selector.ImageMapName != "" {
		im = &v1alpha1.ImageMap{}
		err := r.client.Get(ctx, types.NamespacedName{Name: selector.ImageMapName}, im)
		if err != nil {
			// Do nothing if an object hasn't appeared yet.
			return false, client.IgnoreNotFound(err)
		}

		if monitor.lastImageMapStatus == nil ||
			!apicmp.DeepEqual(monitor.lastImageMapStatus, &(im.Status)) {
			changed = true
		}
	}

	if im == nil {
		monitor.lastImageMapStatus = nil
	} else {
		monitor.lastImageMapStatus = &(im.Status)
	}

	if ka == nil {
		monitor.lastKubernetesApplyStatus = nil
	} else {
		monitor.lastKubernetesApplyStatus = &(ka.Status)
	}

	monitor.lastKubernetesDiscovery = kd

	return changed, nil
}

// Convert the currently tracked state into a set of inputs
// to the updater, then apply them.
func (r *Reconciler) maybeSync(ctx context.Context, lu *v1alpha1.LiveUpdate, monitor *monitor) error {
	// TODO(nick): Fill this in.
	return nil
}

// Live-update containers by copying files and running exec commands.
//
// Update the apiserver when finished.
//
// We expose this as a public method as a hack! Currently, in Tilt, BuildController
// decides when to kick off the live update, and run a full image build+deploy if it
// fails. Eventually we'll invert that relationship, so that BuildController
// (and other API reconcilers) watch the live update API.
func (r *Reconciler) ForceApply(
	ctx context.Context,
	nn types.NamespacedName,
	spec v1alpha1.LiveUpdateSpec,
	input Input) (v1alpha1.LiveUpdateStatus, error) {
	var obj v1alpha1.LiveUpdate
	err := r.client.Get(ctx, nn, &obj)
	if err != nil {
		return v1alpha1.LiveUpdateStatus{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.apply(ctx, &obj, spec, input, nil)
}

// Helper function for the two live update codepaths
// (the reconciler path and the ForceApply path).
// Assumes we hold the lock.
func (r *Reconciler) apply(
	ctx context.Context,
	obj *v1alpha1.LiveUpdate,
	spec v1alpha1.LiveUpdateSpec,
	input Input,
	monitor *monitor) (v1alpha1.LiveUpdateStatus, error) {
	status := r.applyInternal(ctx, spec, input)

	if monitor != nil {
		for _, c := range status.Containers {
			monitor.containers[monitorContainerKey{
				containerID: c.ContainerID,
				podName:     c.PodName,
				namespace:   c.Namespace,
			}] = monitorContainerStatus{
				lastFileTimeSynced: c.LastFileTimeSynced,
				unrecoverable:      status.Failed != nil,
			}
		}
	}

	// Check to see if this is a state transition.
	if status.Failed != nil {
		transitionTime := apis.NowMicro()
		if obj.Status.Failed != nil && obj.Status.Failed.Reason == status.Failed.Reason {
			// If the reason hasn't changed, don't treat this as a transition.
			transitionTime = obj.Status.Failed.LastTransitionTime
		}
		status.Failed.LastTransitionTime = transitionTime
	}

	if !apicmp.DeepEqual(status, obj.Status) {
		update := obj.DeepCopy()
		update.Status = status
		err := r.client.Status().Update(ctx, update)
		if err != nil {
			return v1alpha1.LiveUpdateStatus{}, err
		}
	}

	return status, nil
}

// Like apply, but doesn't write the status to the apiserver.
func (r *Reconciler) applyInternal(
	ctx context.Context,
	spec v1alpha1.LiveUpdateSpec,
	input Input) v1alpha1.LiveUpdateStatus {

	var result v1alpha1.LiveUpdateStatus
	cu := r.containerUpdater(input)
	l := logger.Get(ctx)
	containers := input.Containers
	cIDStr := container.ShortStrs(liveupdates.IDsForContainers(containers))
	suffix := ""
	if len(containers) != 1 {
		suffix = "(s)"
	}

	runSteps := liveupdate.RunSteps(spec)
	changedFiles := input.ChangedFiles
	hotReload := !liveupdate.ShouldRestart(spec)
	boiledSteps, err := build.BoilRuns(runSteps, changedFiles)
	if err != nil {
		result.Failed = &v1alpha1.LiveUpdateStateFailed{
			Reason:  "Invalid",
			Message: fmt.Sprintf("Building exec: %v", err),
		}
		return result
	}

	// rm files from container
	toRemove, toArchive, err := build.MissingLocalPaths(ctx, changedFiles)
	if err != nil {
		result.Failed = &v1alpha1.LiveUpdateStateFailed{
			Reason:  "Invalid",
			Message: fmt.Sprintf("Mapping paths: %v", err),
		}
		return result
	}

	if len(toRemove) > 0 {
		l.Infof("Will delete %d file(s) from container%s: %s", len(toRemove), suffix, cIDStr)
		for _, pm := range toRemove {
			l.Infof("- '%s' (matched local path: '%s')", pm.ContainerPath, pm.LocalPath)
		}
	}

	if len(toArchive) > 0 {
		l.Infof("Will copy %d file(s) to container%s: %s", len(toArchive), suffix, cIDStr)
		for _, pm := range toArchive {
			l.Infof("- %s", pm.PrettyStr())
		}
	}

	var lastExecErrorStatus *v1alpha1.LiveUpdateContainerStatus
	for _, cInfo := range containers {
		archive := build.TarArchiveForPaths(ctx, toArchive, nil)
		err = cu.UpdateContainer(ctx, cInfo, archive,
			build.PathMappingsToContainerPaths(toRemove), boiledSteps, hotReload)

		lastFileTimeSynced := input.LastFileTimeSynced
		if lastFileTimeSynced.IsZero() {
			lastFileTimeSynced = apis.NowMicro()
		}

		cStatus := v1alpha1.LiveUpdateContainerStatus{
			ContainerName:      cInfo.ContainerName.String(),
			ContainerID:        cInfo.ContainerID.String(),
			PodName:            cInfo.PodID.String(),
			Namespace:          cInfo.Namespace.String(),
			LastFileTimeSynced: lastFileTimeSynced,
		}

		if err != nil {
			if runFail, ok := build.MaybeRunStepFailure(err); ok {
				// Keep running updates -- we want all containers to have the same files on them
				// even if the Runs don't succeed
				logger.Get(ctx).Infof("  → Failed to update container %s: run step %q failed with exit code: %d",
					cInfo.ContainerID.ShortStr(), runFail.Cmd.String(), runFail.ExitCode)
				cStatus.LastExecError = err.Error()
				lastExecErrorStatus = &cStatus
			} else {
				// Something went wrong with this update and it's NOT the user's fault--
				// likely a infrastructure error. Bail, and fall back to full build.
				result.Failed = &v1alpha1.LiveUpdateStateFailed{
					Reason:  "UpdateFailed",
					Message: fmt.Sprintf("Updating pod %s: %v", cStatus.PodName, err),
				}
				return result
			}
		} else {
			logger.Get(ctx).Infof("  → Container %s updated!", cInfo.ContainerID.ShortStr())
			if lastExecErrorStatus != nil {
				// This build succeeded, but previously at least one failed due to user error.
				// We may have inconsistent state--bail, and fall back to full build.
				result.Failed = &v1alpha1.LiveUpdateStateFailed{
					Reason: "PodsInconsistent",
					Message: fmt.Sprintf("Pods in inconsistent state. Success: pod %s. Failure: pod %s. Error: %v",
						cStatus.PodName, lastExecErrorStatus.PodName, lastExecErrorStatus.LastExecError),
				}
				return result
			}
		}

		result.Containers = append(result.Containers, cStatus)
	}
	return result
}

func (r *Reconciler) containerUpdater(input Input) containerupdate.ContainerUpdater {
	isDC := input.IsDC
	if isDC || r.updateMode == liveupdates.UpdateModeContainer {
		return r.DockerUpdater
	}

	if r.updateMode == liveupdates.UpdateModeKubectlExec {
		return r.ExecUpdater
	}

	dcu, ok := r.DockerUpdater.(*containerupdate.DockerUpdater)
	if ok && dcu.WillBuildToKubeContext(r.kubeContext) {
		return r.DockerUpdater
	}

	return r.ExecUpdater
}

func (r *Reconciler) CreateBuilder(mgr ctrl.Manager) (*builder.Builder, error) {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.LiveUpdate{}).
		Watches(&source.Kind{Type: &v1alpha1.KubernetesDiscovery{}},
			handler.EnqueueRequestsFromMapFunc(r.indexer.Enqueue)).
		Watches(&source.Kind{Type: &v1alpha1.KubernetesApply{}},
			handler.EnqueueRequestsFromMapFunc(r.indexer.Enqueue)).
		Watches(&source.Kind{Type: &v1alpha1.FileWatch{}},
			handler.EnqueueRequestsFromMapFunc(r.indexer.Enqueue)).
		Watches(&source.Kind{Type: &v1alpha1.ImageMap{}},
			handler.EnqueueRequestsFromMapFunc(r.indexer.Enqueue))

	return b, nil
}

// indexLiveUpdate returns keys of objects referenced _by_ the LiveUpdate object for reverse lookup including:
//  - FileWatch
//  - ImageMapName
// 	- KubernetesDiscovery
//	- KubernetesApply
func indexLiveUpdate(obj ctrlclient.Object) []indexer.Key {
	lu := obj.(*v1alpha1.LiveUpdate)
	var result []indexer.Key

	for _, fwn := range lu.Spec.FileWatchNames {
		result = append(result, indexer.Key{
			Name: types.NamespacedName{
				Namespace: lu.Namespace,
				Name:      fwn,
			},
			GVK: fwGVK,
		})
	}

	if lu.Spec.Selector.Kubernetes != nil {
		if lu.Spec.Selector.Kubernetes.DiscoveryName != "" {
			result = append(result, indexer.Key{
				Name: types.NamespacedName{
					Namespace: lu.Namespace,
					Name:      lu.Spec.Selector.Kubernetes.DiscoveryName,
				},
				GVK: discoveryGVK,
			})
		}

		if lu.Spec.Selector.Kubernetes.ApplyName != "" {
			result = append(result, indexer.Key{
				Name: types.NamespacedName{
					Namespace: lu.Namespace,
					Name:      lu.Spec.Selector.Kubernetes.ApplyName,
				},
				GVK: applyGVK,
			})
		}

		if lu.Spec.Selector.Kubernetes.ImageMapName != "" {
			result = append(result, indexer.Key{
				Name: types.NamespacedName{
					Namespace: lu.Namespace,
					Name:      lu.Spec.Selector.Kubernetes.ImageMapName,
				},
				GVK: imageMapGVK,
			})
		}
	}
	return result
}