package job

import (
	"context"
	"github.com/spf13/viper"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
)

// Watcher has client of kubernetes and target container information.
type Watcher struct {
	client kubernetes.Interface

	// Target container name.
	Container string
}

// NewWatcher returns a new Watcher struct.
func NewWatcher(client kubernetes.Interface, container string) *Watcher {
	return &Watcher{
		client,
		container,
	}
}

// Watch gets pods and tail the logs.
// We must create endless loop because sometimes jobs are configured restartPolicy.
// When restartPolicy is Never, the Job create a new Pod if the specified command is failed.
// So we must trace all Pods even though the Pod is failed.
// And it isn't necessary to stop the loop because the Job is watched in WaitJobComplete.
func (w *Watcher) Watch(job *v1.Job, ctx context.Context) error {
	currentPodList := []corev1.Pod{}
retry:
	for {
		var currentPodNames []string
		for _, pod := range currentPodList {
			currentPodNames = append(currentPodNames, pod.Name)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"job":             job.Name,
			"currentPodNames": currentPodNames,
		}).Info("Watch continue retry:")

		newPodList, err := w.FindPods(ctx, job)
		var newPodNames []string
		if err == nil {
			for _, pod := range newPodList {
				newPodNames = append(newPodNames, pod.Name)
			}
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"job":         job.Name,
			"newPodNames": newPodNames,
			"err":         err,
		}).Debug("FindPods")
		if err != nil {
			return err
		}

		incrementalPodList := diffPods(currentPodList, newPodList)
		var incrementalPodNames []string
		for _, pod := range incrementalPodList {
			incrementalPodNames = append(incrementalPodNames, pod.Name)
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"job":                 job.Name,
			"currentPodNames":     currentPodNames,
			"newPodNames":         newPodNames,
			"incrementalPodNames": incrementalPodNames,
		}).Debug("diffPods")
		go w.WatchPods(ctx, incrementalPodList)

		time.Sleep(1 * time.Second)
		currentPodList = newPodList
		continue retry
	}
}

// WatchPods gets wait to start pod and tail the logs.
func (w *Watcher) WatchPods(ctx context.Context, pods []corev1.Pod) error {
	log.WithContext(ctx).WithFields(log.Fields{
		"pods": pods,
	}).Debug("WatchPods start")

	var wg sync.WaitGroup
	errCh := make(chan error, len(pods))

	for _, pod := range pods {
		log.WithContext(ctx).WithFields(log.Fields{
			"pod.Name": pod.Name,
		}).Debug("range pod")
		wg.Add(1)
		go func(p corev1.Pod) {
			log.WithContext(ctx).WithFields(log.Fields{
				"pod.Name": p.Name,
			}).Debug("go func")
			defer wg.Done()
			startedPod, err := w.WaitToStartPod(ctx, p)
			log.WithContext(ctx).WithFields(log.Fields{
				"startedPod.Name": startedPod.Name,
				"err":             err,
			}).Debug("WaitToStartPod finished")
			if err != nil {
				errCh <- err
				return
			}
			// Ref: https://github.com/kubernetes/client-go/blob/03bfb9bdcfe5482795b999f39ca3ed9ad42ce5bb/kubernetes/typed/core/v1/pod_expansion.go
			logOptions := corev1.PodLogOptions{
				Container: w.Container,
				Follow:    true,
			}
			// Ref: https://stackoverflow.com/questions/32983228/kubernetes-go-client-api-for-log-of-a-particular-pod
			request := w.client.CoreV1().Pods(startedPod.Namespace).GetLogs(startedPod.Name, &logOptions).
				Param("follow", strconv.FormatBool(true)).
				Param("container", w.Container).
				Param("timestamps", strconv.FormatBool(false))
			log.WithContext(ctx).WithFields(log.Fields{
				"startedPod.Name": startedPod.Name,
				"request":         request,
			}).Debug("readStreamLog start")
			err = readStreamLog(ctx, request, startedPod)
			log.WithContext(ctx).WithFields(log.Fields{
				"startedPod.Name": startedPod.Name,
				"err":             err,
			}).Debug("readStreamLog finished")
			errCh <- err
		}(pod)
	}

	select {
	case err := <-errCh:
		if err != nil {
			log.Error(err)
			log.WithContext(ctx).WithFields(log.Fields{
				"err": err,
			}).Debug("WatchPods error")
			return err
		}
	}
	log.WithContext(ctx).Debug("WatchPods wait")
	wg.Wait()
	log.WithContext(ctx).Debug("WatchPods finished")
	return nil
}

// FindPods finds pods in the job.
func (w *Watcher) FindPods(ctx context.Context, job *v1.Job) ([]corev1.Pod, error) {
	labels := parseLabels(job.Spec.Template.Labels)
	listOptions := metav1.ListOptions{
		LabelSelector: labels,
	}
	podList, err := w.client.CoreV1().Pods(job.Namespace).List(ctx, listOptions)
	if err != nil {
		return []corev1.Pod{}, err
	}
	return podList.Items, err
}

// WaitToStartPod wait until starting the pod.
// Because the job does not start immediately after call kubernetes API.
// So we have to wait to start the pod, before watch logs.
func (w *Watcher) WaitToStartPod(ctx context.Context, pod corev1.Pod) (corev1.Pod, error) {
	log.WithContext(ctx).WithFields(log.Fields{
		"pod": pod,
	}).Debug("WaitToStartPod start")

retry:
	for {
		targetPod, err := w.client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		log.WithContext(ctx).WithFields(log.Fields{
			"pod.Name": pod.Name,
			"err":      err,
		}).Debug("get pod")
		if err != nil {
			return pod, err
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"targetPod.Name":         targetPod.Name,
			"targetPod.Status.Phase": targetPod.Status.Phase,
		}).Debug("pod status")

		if !isPendingPod(*targetPod) {
			return *targetPod, nil
		}
		time.Sleep(1 * time.Second)
		continue retry
	}
}

// isPendingPod check the pods whether it have pending container.
func isPendingPod(pod corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodPending {
		return true
	}
	return false
}

// parseLabels parses label sets, and build query string.
func parseLabels(labels map[string]string) string {
	query := []string{}
	for k, v := range labels {
		query = append(query, k+"="+v)
	}
	return strings.Join(query, ",")
}

// readStreamLog reads rest request, and output the log to stdout with stream.
func readStreamLog(ctx context.Context, request *restclient.Request, pod corev1.Pod) error {
	readCloser, err := request.Stream(ctx)
	if err != nil {
		return err
	}
	defer readCloser.Close()
	if viper.GetBool("verbose") {
		buf := new(strings.Builder)
		_, err := io.Copy(buf, readCloser)
		if err != nil {
			return err
		}
		log.WithFields(log.Fields{
			"pod.Name": pod.Name,
			"log":      buf.String(),
		}).Debug("readStreamLog")
		return nil
	}
	_, err = io.Copy(os.Stdout, readCloser)
	return err
}

// diffPods returns diff between the two pods list.
// It returns newPodList - currentPodList, which is incremental Pods list.
func diffPods(currentPodList, newPodList []corev1.Pod) []corev1.Pod {
	var diff []corev1.Pod

	for _, newPod := range newPodList {
		found := false
		for _, currentPod := range currentPodList {
			if currentPod.Name == newPod.Name {
				found = true
				break
			}
		}
		if !found {
			diff = append(diff, newPod)
		}
	}
	return diff
}
