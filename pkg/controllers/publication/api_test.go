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

package publication

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/google/go-cmp/cmp"

	"github.com/pkg/errors"

	"github.com/crossplane/crossplane-runtime/pkg/test"

	"github.com/crossplane/crossplane/apis/apiextensions/v1alpha1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRender(t *testing.T) {
	type args struct {
		kube client.Client
		ip   v1alpha1.InfrastructurePublication
	}
	type want struct {
		crd *apiextensions.CustomResourceDefinition
		err error
	}
	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"GetCRDFailed": {
			reason: "We should return error if CRD cannot be found",
			args: args{
				kube: &test.MockClient{
					MockGet: test.NewMockGetFn(errBoom),
				},
			},
			want: want{
				err: errors.Wrap(errBoom, errGetCRD),
			},
		},
		"RenderedCorrectly": {
			reason: "A proper CRD should be rendered without UID, timestamps etc.",
			args: args{
				kube: &test.MockClient{
					MockGet: func(_ context.Context, _ client.ObjectKey, obj runtime.Object) error {
						c := &apiextensions.CustomResourceDefinition{
							ObjectMeta: metav1.ObjectMeta{
								UID: "my-little-uid",
							},
							Spec: apiextensions.CustomResourceDefinitionSpec{
								Group: "mygroup",
							},
						}
						c.DeepCopyInto(obj.(*apiextensions.CustomResourceDefinition))
						return nil
					},
				},
			},
			want: want{
				crd: &apiextensions.CustomResourceDefinition{
					Spec: apiextensions.CustomResourceDefinitionSpec{
						Group: "mygroup",
					},
				},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewAPIRemoteCRDRenderer(tc.args.kube)
			got, err := r.Render(context.Background(), tc.args.ip)

			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("\nReason: %s\nRender(...): -want error, +got error:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.crd, got); diff != "" {
				t.Errorf("\nReason: %s\nRender(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}
