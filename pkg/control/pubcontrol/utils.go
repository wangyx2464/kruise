/*
Copyright 2021 The Kruise Authors.

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

package pubcontrol

import (
	"context"
	"fmt"
	"time"

	policyv1alpha1 "github.com/openkruise/kruise/apis/policy/v1alpha1"
	kubeClient "github.com/openkruise/kruise/pkg/client"
	"github.com/openkruise/kruise/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// MaxUnavailablePodSize is the max size of PUB.DisruptedPods + PUB.UnavailablePods.
	MaxUnavailablePodSize = 2000
)

var ConflictRetry = wait.Backoff{
	Steps:    4,
	Duration: 500 * time.Millisecond,
	Factor:   1.0,
	Jitter:   0.1,
}

type Operation string

const (
	UpdateOperation = "UPDATE"
	//DeleteOperation = "DELETE"

	// Marked pods will not be pub-protected, solving the scenario of force pod deletion
	PodPubNoProtectionAnnotation = "pub.kruise.io/no-protect"

	// related-pub annotation in pod
	PodRelatedPubAnnotation = "kruise.io/related-pub"
)

// parameters:
// 1. allowed(bool) indicates whether to allow this update operation
// 2. err(error)
func PodUnavailableBudgetValidatePod(client client.Client, control PubControl, pub *policyv1alpha1.PodUnavailableBudget, pod *corev1.Pod, operation Operation, dryRun bool) (allowed bool, reason string, err error) {
	// If the pod is not ready, it doesn't count towards healthy and we should not decrement
	if !control.IsPodReady(pod) {
		klog.V(3).Infof("pod(%s/%s) is not ready, then don't need check pub", pod.Namespace, pod.Name)
		return true, "", nil
	}
	// pod is in pub.Status.DisruptedPods or pub.Status.UnavailablePods, then don't need check it
	if isPodRecordedInPub(pod.Name, pub) {
		klog.V(5).Infof("pod(%s/%s) already is recorded in pub(%s/%s)", pod.Namespace, pod.Name, pub.Namespace, pub.Name)
		return true, "", nil
	}

	// for debug
	var conflictTimes int
	var costOfGet, costOfUpdate time.Duration

	refresh := false
	var pubClone *policyv1alpha1.PodUnavailableBudget
	err = retry.RetryOnConflict(ConflictRetry, func() error {
		unlock := util.GlobalKeyedMutex.Lock(string(pub.UID))
		defer unlock()

		start := time.Now()
		if refresh {
			pubClone, err = kubeClient.GetGenericClient().KruiseClient.PolicyV1alpha1().
				PodUnavailableBudgets(pub.Namespace).Get(context.TODO(), pub.Name, metav1.GetOptions{})
			if err != nil {
				klog.Errorf("Get PodUnavailableBudget(%s/%s) failed form etcd: %s", pub.Namespace, pub.Name, err.Error())
				return err
			}
		} else {
			// compare local cache and informer cache, then get the newer one
			item, _, err := util.GlobalCache.Get(pub)
			if err != nil {
				klog.Errorf("Get cache failed for PodUnavailableBudget(%s/%s): %s", pub.Namespace, pub.Name, err.Error())
			}
			if localCached, ok := item.(*policyv1alpha1.PodUnavailableBudget); ok {
				pubClone = localCached.DeepCopy()
			} else {
				pubClone = pub.DeepCopy()
			}

			informerCached := &policyv1alpha1.PodUnavailableBudget{}
			if err := client.Get(context.TODO(), types.NamespacedName{Namespace: pub.Namespace,
				Name: pub.Name}, informerCached); err == nil {
				var localRV, informerRV int64
				_ = runtime.Convert_string_To_int64(&pubClone.ResourceVersion, &localRV, nil)
				_ = runtime.Convert_string_To_int64(&informerCached.ResourceVersion, &informerRV, nil)
				if informerRV > localRV {
					pubClone = informerCached
				}
			}
		}
		costOfGet += time.Since(start)

		// Try to verify-and-decrement
		// If it was false already, or if it becomes false during the course of our retries,
		err := checkAndDecrement(pod.Name, pubClone, operation)
		if err != nil {
			return err
		}

		// If this is a dry-run, we don't need to go any further than that.
		if dryRun {
			klog.V(5).Infof("pod(%s) operation for pub(%s/%s) is a dry run", pod.Name, pubClone.Namespace, pubClone.Name)
			return nil
		}
		klog.V(3).Infof("pub(%s/%s) update status(disruptedPods:%d, unavailablePods:%d, expectedCount:%d, desiredAvailable:%d, currentAvailable:%d, unavailableAllowed:%d)",
			pubClone.Namespace, pubClone.Name, len(pubClone.Status.DisruptedPods), len(pubClone.Status.UnavailablePods),
			pubClone.Status.TotalReplicas, pubClone.Status.DesiredAvailable, pubClone.Status.CurrentAvailable, pubClone.Status.UnavailableAllowed)
		start = time.Now()
		err = client.Status().Update(context.TODO(), pubClone)
		costOfUpdate += time.Since(start)
		if err == nil {
			if err = util.GlobalCache.Add(pubClone); err != nil {
				klog.Errorf("Add cache failed for PodUnavailableBudget(%s/%s): %s", pub.Namespace, pub.Name, err.Error())
			}
			return nil
		}
		// if conflict, then retry
		conflictTimes++
		refresh = true
		return err
	})
	klog.V(3).Infof("Webhook cost of pub(%s/%s): conflict times %v, cost of Get %v, cost of Update %v",
		pub.Namespace, pub.Name, conflictTimes, costOfGet, costOfUpdate)
	if err != nil && err != wait.ErrWaitTimeout {
		klog.V(3).Infof("pod(%s/%s) operation(%s) for pub(%s/%s) failed: %s", pod.Namespace, pod.Name, operation, pub.Namespace, pub.Name, err.Error())
		return false, err.Error(), nil
	} else if err == wait.ErrWaitTimeout {
		err = errors.NewTimeoutError(fmt.Sprintf("couldn't update PodUnavailableBudget %s due to conflicts", pub.Name), 10)
		klog.Errorf("pod(%s/%s) operation(%s) failed: %s", pod.Namespace, pod.Name, operation, err.Error())
		return false, err.Error(), nil
	}

	klog.V(3).Infof("admit pod(%s/%s) operation(%s) for pub(%s/%s)", pod.Namespace, pod.Name, operation, pub.Namespace, pub.Name)
	return true, "", nil
}

func checkAndDecrement(podName string, pub *policyv1alpha1.PodUnavailableBudget, operation Operation) error {
	if pub.Status.UnavailableAllowed <= 0 {
		return errors.NewForbidden(policyv1alpha1.Resource("podunavailablebudget"), pub.Name, fmt.Errorf("pub unavailable allowed is negative"))
	}
	if len(pub.Status.DisruptedPods)+len(pub.Status.UnavailablePods) > MaxUnavailablePodSize {
		return errors.NewForbidden(policyv1alpha1.Resource("podunavailablebudget"), pub.Name, fmt.Errorf("DisruptedPods and UnavailablePods map too big - too many unavailable not confirmed by PUB controller"))
	}

	pub.Status.UnavailableAllowed--

	if pub.Status.DisruptedPods == nil {
		pub.Status.DisruptedPods = make(map[string]metav1.Time)
	}
	if pub.Status.UnavailablePods == nil {
		pub.Status.UnavailablePods = make(map[string]metav1.Time)
	}

	if operation == UpdateOperation {
		pub.Status.UnavailablePods[podName] = metav1.Time{Time: time.Now()}
		klog.V(3).Infof("pod(%s) is recorded in pub(%s/%s) UnavailablePods", podName, pub.Namespace, pub.Name)
	} else {
		pub.Status.DisruptedPods[podName] = metav1.Time{Time: time.Now()}
		klog.V(3).Infof("pod(%s) is recorded in pub(%s/%s) DisruptedPods", podName, pub.Namespace, pub.Name)
	}
	return nil
}

func isPodRecordedInPub(podName string, pub *policyv1alpha1.PodUnavailableBudget) bool {
	if _, ok := pub.Status.UnavailablePods[podName]; ok {
		return true
	}
	if _, ok := pub.Status.DisruptedPods[podName]; ok {
		return true
	}
	return false
}
