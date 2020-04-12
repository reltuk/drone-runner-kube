// Copyright 2019 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/drone-runners/drone-runner-kube/internal/docker/image"
	"github.com/drone/runner-go/livelog"
	"github.com/hashicorp/go-multierror"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	watchtools "k8s.io/client-go/tools/watch"
)

// Kubernetes implements a Kubernetes pipeline engine.
type Kubernetes struct {
	client *kubernetes.Clientset
}

// NewFromConfig returns a new out-of-cluster engine.
func NewFromConfig(path string) (*Kubernetes, error) {
	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Kubernetes{
		client: clientset,
	}, nil
}

// NewInCluster returns a new in-cluster engine.
func NewInCluster() (*Kubernetes, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Kubernetes{
		client: clientset,
	}, nil
}

// Setup the pipeline environment.
func (k *Kubernetes) Setup(ctx context.Context, spec *Spec) error {
	if spec.PullSecret != nil {
		_, err := k.client.CoreV1().Secrets(spec.PodSpec.Namespace).Create(toDockerConfigSecret(spec))
		if err != nil {
			return err
		}
	}

	_, err := k.client.CoreV1().Secrets(spec.PodSpec.Namespace).Create(toSecret(spec))
	if err != nil {
		return err
	}

	_, err = k.client.CoreV1().Pods(spec.PodSpec.Namespace).Create(toPod(spec))
	if err != nil {
		return err
	}

	return nil
}

// Destroy the pipeline environment.
func (k *Kubernetes) Destroy(ctx context.Context, spec *Spec) error {
	var result error

	if spec.PullSecret != nil {
		err := k.client.CoreV1().Secrets(spec.PodSpec.Namespace).Delete(spec.PullSecret.Name, &metav1.DeleteOptions{})
		if err != nil {
			result = multierror.Append(result, err)
		}
	}

	err := k.client.CoreV1().Secrets(spec.PodSpec.Namespace).Delete(spec.PodSpec.Name, &metav1.DeleteOptions{})
	if err != nil {
		result = multierror.Append(result, err)
	}

	err = k.client.CoreV1().Pods(spec.PodSpec.Namespace).Delete(spec.PodSpec.Name, &metav1.DeleteOptions{})
	if err != nil {
		result = multierror.Append(result, err)
	}

	return result
}

// Run runs the pipeline step.
func (k *Kubernetes) Run(ctx context.Context, spec *Spec, step *Step, output io.Writer) (*State, error) {
	err := k.start(spec, step)
	if err != nil {
		return nil, err
	}

	err = k.waitForReady(ctx, spec, step)
	if err != nil {
		return nil, err
	}

	err = k.tail(ctx, spec, step, output)
	if err != nil {
		return nil, err
	}

	return k.waitForTerminated(ctx, spec, step)
}

func (k *Kubernetes) waitFor(ctx context.Context, spec *Spec, conditionFunc func(e watch.Event) (bool, error)) error {
	label := fmt.Sprintf("io.drone.name=%s", spec.PodSpec.Name)
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return k.client.CoreV1().Pods(spec.PodSpec.Namespace).List(metav1.ListOptions{
				LabelSelector: label,
			})
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return k.client.CoreV1().Pods(spec.PodSpec.Namespace).Watch(metav1.ListOptions{
				LabelSelector: label,
			})
		},
	}

	preconditionFunc := func(store cache.Store) (bool, error) {
		_, exists, err := store.Get(&metav1.ObjectMeta{Namespace: spec.PodSpec.Namespace, Name: spec.PodSpec.Name})
		if err != nil {
			return true, err
		}
		if !exists {
			return true, err
		}
		return false, nil
	}

	_, err := watchtools.UntilWithSync(ctx, lw, &v1.Pod{}, preconditionFunc, conditionFunc)
	return err
}

func (k *Kubernetes) waitForReady(ctx context.Context, spec *Spec, step *Step) error {
	return k.waitFor(ctx, spec, func(e watch.Event) (bool, error) {
		switch t := e.Type; t {
		case watch.Added, watch.Modified:
			pod, ok := e.Object.(*v1.Pod)
			if !ok || pod.ObjectMeta.Name != spec.PodSpec.Name {
				return false, nil
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name != step.ID {
					continue
				}
				if (!image.Match(cs.Image, step.Placeholder) && cs.State.Running != nil) || (cs.State.Terminated != nil) {
					return true, nil
				}
			}
		}
		return false, nil
	})
}

func (k *Kubernetes) waitForTerminated(ctx context.Context, spec *Spec, step *Step) (*State, error) {
	state := &State{
		Exited:    true,
		OOMKilled: false,
	}
	err := k.waitFor(ctx, spec, func(e watch.Event) (bool, error) {
		switch t := e.Type; t {
		case watch.Added, watch.Modified:
			pod, ok := e.Object.(*v1.Pod)
			if !ok || pod.ObjectMeta.Name != spec.PodSpec.Name {
				return false, nil
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name != step.ID {
					continue
				}
				if cs.State.Terminated != nil {
					state.ExitCode = int(cs.State.Terminated.ExitCode)
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}

	return state, nil
}

func (k *Kubernetes) tail(ctx context.Context, spec *Spec, step *Step, output io.Writer) error {
	opts := &v1.PodLogOptions{
		Follow:    true,
		Container: step.ID,
	}

	req := k.client.CoreV1().RESTClient().Get().
		Namespace(spec.PodSpec.Namespace).
		Name(spec.PodSpec.Name).
		Resource("pods").
		SubResource("log").
		VersionedParams(opts, scheme.ParameterCodec)

	readCloser, err := req.Stream()
	if err != nil {
		return err
	}
	defer readCloser.Close()

	return livelog.Copy(output, readCloser)
}

func (k *Kubernetes) start(spec *Spec, step *Step) error {
	orig, err := k.client.CoreV1().Pods(spec.PodSpec.Namespace).Get(spec.PodSpec.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	updated := orig.DeepCopy()

	for i, container := range updated.Spec.Containers {
		if container.Name == step.ID {
			updated.Spec.Containers[i].Image = step.Image
			if updated.ObjectMeta.Annotations == nil {
				updated.ObjectMeta.Annotations = map[string]string{}
			}
			for _, env := range statusesWhiteList {
				updated.ObjectMeta.Annotations[env] = step.Envs[env]
			}
		}
	}

	origJson, err := json.Marshal(orig)
	if err != nil {
		return err
	}

	updatedJson, err := json.Marshal(updated)
	if err != nil {
		return err
	}

	patch, err := strategicpatch.CreateTwoWayMergePatch(origJson, updatedJson, v1.Pod{})
	if err != nil {
		return  err
	}

	_, err = k.client.CoreV1().Pods(spec.PodSpec.Namespace).Patch(orig.Name, types.StrategicMergePatchType, patch)
	return err
}
