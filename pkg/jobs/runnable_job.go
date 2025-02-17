package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

var defaultResyncDuration = 30 * time.Minute

type runnableJob struct {
	clientset  kubernetes.Interface
	logsReader LogsReader
	job        *batchv1.Job // job to be run
}

// NewRunnableJob constructs a new Runnable task defined as Kubernetes
func NewRunnableJob(
	clientset kubernetes.Interface,
	job *batchv1.Job,
) Runnable {
	return &runnableJob{
		clientset:  clientset,
		logsReader: NewLogsReader(clientset),
		job:        job,
	}
}

// Run runs synchronously the task as Kubernetes job.
// This method blocks and waits for the job completion or failure.
func (r *runnableJob) Run(ctx context.Context) error {
	var err error
	r.job, err = r.clientset.BatchV1().Jobs(r.job.Namespace).Create(ctx, r.job, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		r.clientset,
		defaultResyncDuration,
		informers.WithNamespace(r.job.Namespace),
	)
	jobsInformer := informerFactory.Batch().V1().Jobs()
	complete := make(chan error)

	_, err = jobsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, newObj interface{}) {
			newJob, ok := newObj.(*batchv1.Job)
			if !ok {
				return
			}
			if r.job.UID != newJob.UID {
				return
			}
			for _, condition := range newJob.Status.Conditions {
				switch condition.Type {
				case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
					complete <- nil
					return
				case batchv1.JobFailed:
					complete <- fmt.Errorf("job failed: %s: %s", condition.Reason, condition.Message)
					return
				}
			}
		},
	})
	if err != nil {
		return err
	}
	eventsInformer := informerFactory.Core().V1().Events()
	_, err = eventsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			event := obj.(*corev1.Event)
			if event.InvolvedObject.UID != r.job.UID {
				return
			}

			if event.Type == corev1.EventTypeWarning {
				complete <- fmt.Errorf("warning event received: %s (%s)", event.Message, event.Reason)
				return
			}
		},
	})
	if err != nil {
		return err
	}
	informerFactory.Start(wait.NeverStop)
	informerFactory.WaitForCacheSync(wait.NeverStop)

	err = <-complete

	if err != nil {
		r.logTerminatedContainersErrors(ctx)
	}

	return err
}

func (r *runnableJob) logTerminatedContainersErrors(ctx context.Context) {
	statuses, err := r.logsReader.GetTerminatedContainersStatusesByJob(ctx, r.job)
	if err != nil {
		slog.Error(fmt.Sprintf("Error while getting terminated containers statuses for job %q", r.job.Namespace+"/"+r.job.Name))
	}

	for _, status := range statuses {
		if status.ExitCode == 0 {
			continue
		}
	}
}

func GetActiveDeadlineSeconds(d time.Duration) *int64 {
	if d > 0 {
		return ptr.To[int64](int64(d.Seconds()))
	}
	return nil
}
