/*
Copyright 2020 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package requirement

import (
	"context"
	"time"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	rresource "github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured"
	"github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/requirement"

	"github.com/crossplane/agent/pkg/resource"
)

const (
	timeout   = 2 * time.Minute
	longWait  = 1 * time.Minute
	shortWait = 30 * time.Second
	tinyWait  = 5 * time.Second

	finalizer = "agent.crossplane.io/sync"

	local  = "local cluster: "
	remote = "remote cluster: "

	errGetRequirement            = "cannot get requirement"
	errDeleteRequirement         = "cannot delete requirement"
	errCreateRequirement         = "cannot create requirement"
	errUpdateRequirement         = "cannot update requirement"
	errRemoveFinalizer           = "cannot remove finalizer"
	errAddFinalizer              = "cannot add finalizer"
	errGetSecret                 = "cannot get secret"
	errUpdateSecretOfRequirement = "cannot update secret of the requirement"
	errConvertStatusToLocal      = "cannot convert status of the requirement for the local object"
)

type ReconcilerOption func(*Reconciler)

func WithLogger(l logging.Logger) ReconcilerOption {
	return func(r *Reconciler) {
		r.log = l
	}
}

func WithRecorder(rec event.Recorder) ReconcilerOption {
	return func(r *Reconciler) {
		r.record = rec
	}
}

func NewReconciler(mgr manager.Manager, remoteClient client.Client, gvk schema.GroupVersionKind, opts ...ReconcilerOption) *Reconciler {
	ni := func() *requirement.Unstructured { return requirement.New(requirement.WithGroupVersionKind(gvk)) }
	lc := unstructured.NewClient(mgr.GetClient())
	rc := unstructured.NewClient(remoteClient)
	r := &Reconciler{
		mgr: mgr,
		local: rresource.ClientApplicator{
			Client:     lc,
			Applicator: rresource.NewAPIUpdatingApplicator(lc),
		},
		remote: rresource.ClientApplicator{
			Client:     rc,
			Applicator: rresource.NewAPIUpdatingApplicator(rc),
		},
		newInstance: ni,
		log:         logging.NewNopLogger(),
		finalizer:   rresource.NewAPIFinalizer(lc, finalizer),
		record:      event.NewNopRecorder(),
	}

	for _, f := range opts {
		f(r)
	}
	return r
}

type Reconciler struct {
	mgr    ctrl.Manager
	local  rresource.ClientApplicator
	remote rresource.ClientApplicator

	newInstance func() *requirement.Unstructured
	finalizer   rresource.Finalizer

	log    logging.Logger
	record event.Recorder
}

func (r *Reconciler) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	log := r.log.WithValues("request", req)
	log.Debug("Reconciling")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	re := r.newInstance()
	if err := r.local.Get(ctx, req.NamespacedName, re); err != nil {
		if kerrors.IsNotFound(err) {
			return reconcile.Result{Requeue: false}, nil
		}
		return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, local+errGetRequirement)
	}
	reRemote := r.newInstance()
	err := r.remote.Get(ctx, req.NamespacedName, reRemote)
	if rresource.IgnoreNotFound(err) != nil {
		return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, remote+errGetRequirement)
	}
	if meta.WasDeleted(re) {
		if kerrors.IsNotFound(err) {
			if err := r.finalizer.RemoveFinalizer(ctx, re); err != nil {
				return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, local+errRemoveFinalizer)
			}
			return reconcile.Result{}, nil
		}
		if err := r.remote.Delete(ctx, reRemote); rresource.IgnoreNotFound(err) != nil {
			return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, remote+errDeleteRequirement)
		}
		return reconcile.Result{RequeueAfter: tinyWait}, nil
	}

	if err := r.finalizer.AddFinalizer(ctx, re); err != nil {
		return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, local+errAddFinalizer)
	}

	// Update the remote object with latest desired state.
	resource.OverrideInputMetadata(re, reRemote)
	resource.EqualizeRequirementSpec(re, reRemote)
	// TODO(muvaf): Existing APIUpdatingApplicator is no-op if you don't supply
	// UpdateFn and in this case that'd be just a repetition. Find a better way
	// for this call.
	if !meta.WasCreated(reRemote) {
		if err = r.remote.Create(ctx, reRemote); err != nil {
			return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, remote+errCreateRequirement)
		}
	}
	if err := r.remote.Update(ctx, reRemote); err != nil {
		return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, remote+errUpdateRequirement)
	}
	// TODO(muvaf): Update local object only if it's changed after late-init.
	if err := r.local.Update(ctx, re); err != nil {
		return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, local+errUpdateRequirement)
	}

	// Update the local object with latest observation.
	if err := resource.PropagateStatus(reRemote, re); err != nil {
		return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, errConvertStatusToLocal)
	}
	if err := r.local.Status().Update(ctx, re); err != nil {
		return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, local+errUpdateRequirement)
	}
	if re.GetWriteConnectionSecretToReference() == nil {
		return reconcile.Result{RequeueAfter: shortWait}, nil
	}

	// Update the connection secret.
	rs := &v1.Secret{}
	rnn := types.NamespacedName{
		Name:      reRemote.GetWriteConnectionSecretToReference().Name,
		Namespace: reRemote.GetNamespace(),
	}
	err = r.remote.Get(ctx, rnn, rs)
	if rresource.IgnoreNotFound(err) != nil {
		return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, remote+errGetSecret)
	}
	if kerrors.IsNotFound(err) {
		// TODO(muvaf): Set condition to say waiting for secret.
		return reconcile.Result{RequeueAfter: longWait}, nil
	}
	ls := rs.DeepCopy()
	ls.SetName(re.GetWriteConnectionSecretToReference().Name)
	ls.SetNamespace(re.GetNamespace())
	meta.AddOwnerReference(ls, meta.AsController(meta.ReferenceTo(re, re.GroupVersionKind())))
	if err := r.local.Apply(ctx, ls, resource.OverrideGeneratedMetadata); err != nil {
		return reconcile.Result{RequeueAfter: shortWait}, errors.Wrap(err, local+errUpdateSecretOfRequirement)
	}
	return reconcile.Result{RequeueAfter: longWait}, nil
}
