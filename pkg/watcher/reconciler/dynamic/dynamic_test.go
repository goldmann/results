// Copyright 2020 The Tekton Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dynamic

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jonboulle/clockwork"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	pipelineruninformer "github.com/tektoncd/pipeline/pkg/client/injection/informers/pipeline/v1beta1/pipelinerun"
	taskruninformer "github.com/tektoncd/pipeline/pkg/client/injection/informers/pipeline/v1beta1/taskrun"
	rtesting "github.com/tektoncd/pipeline/pkg/reconciler/testing"
	"github.com/tektoncd/results/pkg/internal/test"
	"github.com/tektoncd/results/pkg/watcher/convert"
	"github.com/tektoncd/results/pkg/watcher/reconciler"
	"github.com/tektoncd/results/pkg/watcher/reconciler/annotation"
	pb "github.com/tektoncd/results/proto/v1alpha2/results_go_proto"
	"google.golang.org/protobuf/testing/protocmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicclient "k8s.io/client-go/dynamic/fake"
	"knative.dev/pkg/apis"
	duckv1beta1 "knative.dev/pkg/apis/duck/v1beta1"
	"knative.dev/pkg/controller"
	dynamicinject "knative.dev/pkg/injection/clients/dynamicclient/fake"

	// Needed for informer injection.
	_ "github.com/tektoncd/pipeline/test"
)

type env struct {
	ctx     context.Context
	ctrl    *controller.Impl
	results pb.ResultsClient
	dynamic *dynamicclient.FakeDynamicClient
}

func newEnv(t *testing.T, gvr schema.GroupVersionResource, cfg *reconciler.Config) *env {
	t.Helper()

	// Configures fake tekton clients + informers.
	ctx, _ := rtesting.SetupFakeContext(t)

	results := test.NewResultsClient(t)

	var ctrl *controller.Impl
	switch gvr.String() {
	case apis.KindToResource(taskrun.GroupVersionKind()).String():
		ctrl = NewControllerWithConfig(ctx, results, gvr, taskruninformer.Get(ctx).Informer(), cfg)
	case apis.KindToResource(pipelinerun.GroupVersionKind()).String():
		ctrl = NewControllerWithConfig(ctx, results, gvr, pipelineruninformer.Get(ctx).Informer(), cfg)
	default:
		t.Fatalf("unknown GroupVersionResource: %v", gvr)
	}

	return &env{
		ctx:     ctx,
		ctrl:    ctrl,
		results: results,
		dynamic: dynamicinject.Get(ctx),
	}
}

var (
	taskrun = &v1beta1.TaskRun{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "tekton.dev/v1beta1",
			Kind:       "TaskRun",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "taskrun",
			Namespace:   "ns",
			Annotations: map[string]string{"demo": "demo"},
			UID:         "12345",
		},
		Status: v1beta1.TaskRunStatus{
			Status: duckv1beta1.Status{
				Conditions: duckv1beta1.Conditions{
					apis.Condition{
						Type:   apis.ConditionSucceeded,
						Status: corev1.ConditionTrue,
					},
				},
			},
			TaskRunStatusFields: v1beta1.TaskRunStatusFields{},
		},
		Spec: v1beta1.TaskRunSpec{
			TaskSpec: &v1beta1.TaskSpec{
				Steps: []v1beta1.Step{{
					Script: "echo hello world!",
				}},
			},
		},
	}

	pipelinerun = &v1beta1.PipelineRun{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "tekton.dev/v1beta1",
			Kind:       "PipelineRun",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pipelinerun",
			Namespace:   "ns",
			Annotations: map[string]string{"demo": "demo"},
			UID:         "12345",
		},
		Status: v1beta1.PipelineRunStatus{
			Status: duckv1beta1.Status{
				Conditions: duckv1beta1.Conditions{
					apis.Condition{
						Type:   apis.ConditionSucceeded,
						Status: corev1.ConditionTrue,
					},
				},
			},
			PipelineRunStatusFields: v1beta1.PipelineRunStatusFields{},
		},
		Spec: v1beta1.PipelineRunSpec{
			PipelineSpec: &v1beta1.PipelineSpec{
				Tasks: []v1beta1.PipelineTask{{
					Name: "task",
					TaskSpec: &v1beta1.EmbeddedTask{
						TaskSpec: v1beta1.TaskSpec{
							Steps: []v1beta1.Step{{
								Script: "echo hello world!",
							}},
						},
					},
				}},
			},
		},
	}
)

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	for _, o := range []interface {
		metav1.Object
		schema.ObjectKind
	}{taskrun, pipelinerun} {
		t.Run(o.GroupVersionKind().String(), func(t *testing.T) {
			gvr := apis.KindToResource(o.GroupVersionKind())
			env := newEnv(t, gvr, nil)
			client := env.dynamic.Resource(gvr).Namespace(o.GetNamespace())

			data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
			if err != nil {
				t.Fatalf("ToUnstructured: %v", err)
			}
			u, err := client.Create(ctx, &unstructured.Unstructured{Object: data}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			u, err = client.Get(ctx, o.GetName(), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}

			t.Run("create", func(t *testing.T) {
				u = reconcile(t, env, u)
			})

			t.Run("nop", func(t *testing.T) {
				// This is treated like an update, even though there is no change.
				reconcile(t, env, u)
			})

			t.Run("update", func(t *testing.T) {
				u.SetGeneration(u.GetGeneration() + 1)
				u, err = client.Update(ctx, u, metav1.UpdateOptions{})
				if err != nil {
					t.Fatalf("Update: %v", err)
				}
				reconcile(t, env, u)
			})
		})
	}
}

// reconcile forces a reconcile for the given TaskRun, and returns the newest
// TaskRun post-reconcile.
func reconcile(t *testing.T, env *env, want *unstructured.Unstructured) *unstructured.Unstructured {
	if err := env.ctrl.Reconciler.Reconcile(env.ctx, fmt.Sprintf("%s/%s", want.GetNamespace(), want.GetName())); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Verify that the TaskRun now has a Result annotation associated with it.
	u, err := env.dynamic.Resource(apis.KindToResource(want.GroupVersionKind())).Namespace(want.GetNamespace()).Get(env.ctx, want.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("TaskRun.Get(%s): %v", want.GetName(), err)
	}
	for _, a := range []string{annotation.Result, annotation.Record} {
		if _, ok := u.GetAnnotations()[a]; !ok {
			t.Errorf("annotation %s missing", a)
		}
	}

	// Verify Result data matches TaskRun.
	got, err := env.results.GetRecord(env.ctx, &pb.GetRecordRequest{Name: u.GetAnnotations()[annotation.Record]})
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	// We diff the base since we're storing the current state. We don't include
	// the result annotations since that's part of the "next" state.
	wantpb, err := convert.ToProto(want)
	if err != nil {
		t.Fatalf("convert.ToProto: %v", err)
	}
	if diff := cmp.Diff(wantpb, got.GetData(), protocmp.Transform()); diff != "" {
		t.Errorf("Result diff (-want, +got):\n%s", diff)
	}

	return u
}

func TestDisableCRDUpdate(t *testing.T) {
	ctx := context.Background()
	gvr := apis.KindToResource(taskrun.GroupVersionKind())
	env := newEnv(t, gvr, &reconciler.Config{
		DisableAnnotationUpdate: true,
	})
	client := env.dynamic.Resource(gvr).Namespace(taskrun.GetNamespace())

	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(taskrun)
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}
	u, err := client.Create(ctx, &unstructured.Unstructured{Object: data}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := env.ctrl.Reconciler.Reconcile(env.ctx, taskrun.GetNamespacedName().String()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Since annotation updates are disabled, we do not expect any change to
	// the on-cluster TaskRun.
	got, err := client.Get(ctx, taskrun.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("TaskRun.Get(%s): %v", taskrun.GetName(), err)
	}
	if diff := cmp.Diff(u, got); diff != "" {
		t.Errorf("Did not expect change in TaskRun (-want, +got):\n%s", diff)
	}
}

func TestRunCleanup(t *testing.T) {
	ctx := context.Background()
	for _, o := range []interface {
		metav1.Object
		schema.ObjectKind
		GetNamespacedName() types.NamespacedName
	}{taskrun, pipelinerun} {
		gvr := apis.KindToResource(o.GroupVersionKind())

		t.Run(gvr.String(), func(t *testing.T) {
			data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
			if err != nil {
				t.Fatalf("ToUnstructured: %v", err)
			}
			u := &unstructured.Unstructured{Object: data}
			u.SetOwnerReferences([]metav1.OwnerReference{{Name: "parent"}})

			for _, tc := range []struct {
				gracePeriod time.Duration
				wantDelete  bool
			}{
				{
					gracePeriod: -1 * time.Second,
					wantDelete:  true,
				},
				{
					gracePeriod: 0,
					wantDelete:  false,
				},
				{
					gracePeriod: 1 * time.Second,
					wantDelete:  true,
				},
			} {
				t.Run(fmt.Sprintf("GracePeriod_%v", tc.gracePeriod), func(t *testing.T) {
					env := newEnv(t, gvr, &reconciler.Config{
						CompletedResourceGracePeriod: tc.gracePeriod,
					})
					fakeClock := clockwork.NewFakeClockAt(time.Now())
					clock = fakeClock

					client := env.dynamic.Resource(gvr).Namespace(o.GetNamespace())

					u, err := client.Create(ctx, u, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("Create: %v", err)
					}

					t.Run("noop-OwnerReference", func(t *testing.T) {
						if err := env.ctrl.Reconciler.Reconcile(env.ctx, o.GetNamespacedName().String()); err != nil {
							t.Fatalf("Reconcile: %v", err)
						}

						// First run should be a no-op because of the OwnerReference.
						if _, err := client.Get(ctx, o.GetName(), metav1.GetOptions{}); err != nil {
							t.Fatalf("Get(%s): %v", o.GetName(), err)
						}
					})

					t.Run("delete", func(t *testing.T) {
						// Clear out OwnerRefs to make the object eligible for deletion.
						u.SetOwnerReferences(nil)
						if _, err := client.Update(ctx, u, metav1.UpdateOptions{}); err != nil {
							t.Fatalf("Update: %v", err)
						}
						// Force the wall clock forward (with some extra buffer) to
						// simulate the progression of time past the configured grace
						// period.
						fakeClock.Advance(-1*time.Since(time.Now()) + tc.gracePeriod + time.Minute)

						if err := env.ctrl.Reconciler.Reconcile(env.ctx, o.GetNamespacedName().String()); err != nil {
							t.Fatalf("Reconcile: %v", err)
						}

						// Run should be deleted after reconcile. Unfortunately client-go does not
						// provide fine-grain inspection of requests, so we can't verify the
						// request beyond "has this been deleted".
						// See https://github.com/kubernetes/client-go/issues/693
						_, err := client.Get(ctx, o.GetName(), metav1.GetOptions{})
						if (tc.wantDelete && !errors.IsNotFound(err)) || (!tc.wantDelete && err != nil) {
							t.Fatalf("Get(%s), wantDelete: %t: %v", o.GetName(), tc.wantDelete, err)
						}
					})
				})
			}

		})
	}
}
