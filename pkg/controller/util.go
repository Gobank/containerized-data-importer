package controller

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/kubevirt/containerized-data-importer/pkg/common"
	"k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// return a pvc pointer based on the passed-in work queue key.
func (c *Controller) pvcFromKey(key interface{}) (*v1.PersistentVolumeClaim, error) {
	keyString, ok := key.(string)
	if !ok {
		return nil, fmt.Errorf("pvcFromKey: key object not of type string\n")
	}
	obj, ok, err := c.pvcInformer.GetIndexer().GetByKey(keyString)
	if !ok {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pvcFromKey: Error getting key from cache: %q\n", keyString)
	}
	pvc, ok := obj.(*v1.PersistentVolumeClaim)
	if !ok {
		return nil, fmt.Errorf("pvcFromKey: Object not of type *v1.PersistentVolumeClaim\n")
	}
	return pvc, nil
}

// returns the endpoint string which contains the full path URI of the target object to be copied.
func getEndpoint(pvc *v1.PersistentVolumeClaim) (string, error) {
	ep, found := pvc.Annotations[annEndpoint]
	if !found || ep == "" {
		// annotation was present and is now missing or is blank
		return ep, fmt.Errorf("getEndpoint: annotation %q in pvc %s/%s is missing or is blank\n", annEndpoint, pvc.Namespace, pvc.Name)
	}
	return ep, nil
}

// returns the name of the secret containing endpoint credentials consumed by the importer pod.
// A value of "" implies there are no credentials for the endpoint being used. A returned error
// causes processNextItem() to stop.
func (c *Controller) getSecretName(pvc *v1.PersistentVolumeClaim) (string, error) {
	ns := pvc.Namespace
	name, found := pvc.Annotations[annSecret]
	if !found || name == "" {
		msg := ""
		if !found {
			msg = fmt.Sprintf("getEndpointSecret: annotation %q is missing in pvc %s/%s\n", annSecret, ns, pvc.Name)
		} else {
			msg = fmt.Sprintf("getEndpointSecret: secret name is missing from annotation %q in pvc \"%s/%s\n", annSecret, ns, pvc.Name)
		}
		glog.Info(msg)
		return "", nil // importer pod will not contain secret credentials
	}
	glog.Infof("getEndpointSecret: retrieving Secret \"%s/%s\"\n", ns, name)
	_, err := c.clientset.CoreV1().Secrets(ns).Get(name, metav1.GetOptions{})
	if apierrs.IsNotFound(err) {
		glog.Infof("getEndpointSecret: secret %q defined in pvc \"%s/%s\" is missing. Importer pod will run once this secret is created\n", name, ns, pvc.Name)
		return name, nil
	}
	if err != nil {
		return "", fmt.Errorf("getEndpointSecret: error getting secret %q defined in pvc \"%s/%s\": %v\n", name, ns, pvc.Name, err)
	}
	return name, nil
}

// set the pvc's "status" annotation to the passed-in value.
func (c *Controller) setPVCStatus(pvc *v1.PersistentVolumeClaim, status string) (*v1.PersistentVolumeClaim, error) {
	if val, ok := pvc.Annotations[annStatus]; ok && val == status {
		return pvc, nil // annotation already set
	}
	// don't mutate the original pvc since it's from the shared informer
	pvcClone := pvc.DeepCopy()

	var newPVC *v1.PersistentVolumeClaim
	// loop a few times in case the cloned pvc is stale
	err := wait.PollImmediate(time.Second*1, time.Second*4, func() (bool, error) {
		var err error
		metav1.SetMetaDataAnnotation(&pvcClone.ObjectMeta, annStatus, status)
		newPVC, err = c.clientset.CoreV1().PersistentVolumeClaims(pvc.Namespace).Update(pvcClone)
		if err == nil {
			return true, nil // successful update
		}
		verb := "updating"
		if apierrs.IsConflict(err) { // pvc is likely stale
			pvcClone, err = c.clientset.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(pvc.Name, metav1.GetOptions{})
			if err == nil {
				return false, nil // re-try adding annotation
			}
			verb = "getting"
		}
		return true, fmt.Errorf("setPVCStatus: error %s pvc %s/%s: %v\n", verb, pvc.Namespace, pvc.Name, err)
	})
	return newPVC, err
}

// return a pointer to a pod which is created based on the passed-in endpoint, secret
// name, and pvc. A nil secret means the endpoint credentials are not passed to the
// importer pod.
func (c *Controller) createImporterPod(ep, secretName string, pvc *v1.PersistentVolumeClaim) (*v1.Pod, error) {
	ns := pvc.Namespace
	pod := c.makeImporterPodSpec(ep, secretName, pvc)
	var err error
	pod, err = c.clientset.CoreV1().Pods(ns).Create(pod)
	if err != nil {
		return nil, fmt.Errorf("createImporterPod: Create failed: %v\n", err)
	}
	glog.Infof("importer pod \"%s/%s\" (image tag: %q) created\n", pod.Namespace, pod.Name, c.importerImageTag)
	return pod, nil
}

// return the importer pod spec based on the passed-in endpoint, secret and pvc.
func (c *Controller) makeImporterPodSpec(ep, secret string, pvc *v1.PersistentVolumeClaim) *v1.Pod {
	// importer pod name contains the pvc name
	podName := fmt.Sprintf("%s-%s", common.IMPORTER_PODNAME, pvc.Name)
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
			Annotations: map[string]string{
				annCreatedBy: "yes",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            common.IMPORTER_PODNAME,
					Image:           "docker.io/jcoperh/importer:" + c.importerImageTag,
					ImagePullPolicy: v1.PullAlways,
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "data-path",
							MountPath: common.IMPORTER_DATA_DIR,
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name: "data-path",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc.Name,
							ReadOnly:  false,
						},
					},
				},
			},
		},
	}
	pod.Spec.Containers[0].Env = makeEnv(ep, secret)
	return pod
}

// return the Env portion for the importer container.
func makeEnv(endpoint, secret string) []v1.EnvVar {
	env := []v1.EnvVar{
		{
			Name:  common.IMPORTER_ENDPOINT,
			Value: endpoint,
		},
	}
	if secret != "" {
		env = append(env, v1.EnvVar{
			Name: common.IMPORTER_ACCESS_KEY_ID,
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{
					LocalObjectReference: v1.LocalObjectReference{
						Name: secret,
					},
					Key: common.KeyAccess,
				},
			},
		}, v1.EnvVar{
			Name: common.IMPORTER_SECRET_KEY,
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{
					LocalObjectReference: v1.LocalObjectReference{
						Name: secret,
					},
					Key: common.KeySecret,
				},
			},
		})
	}
	return env
}
