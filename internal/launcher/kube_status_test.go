package launcher

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
)

func statusKube(objs ...any) *Kube {
	cs := fake.NewSimpleClientset()
	for _, o := range objs {
		switch o := o.(type) {
		case *batchv1.Job:
			cs.BatchV1().Jobs("runs").Create(context.Background(), o, metav1.CreateOptions{})
		case *corev1.Pod:
			cs.CoreV1().Pods("runs").Create(context.Background(), o, metav1.CreateOptions{})
		}
	}
	return NewKube(cs, &config.Environment{Runs: config.Runs{Namespace: "runs"}})
}

func job(conds ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "runs"}, Status: batchv1.JobStatus{Conditions: conds}}
}

func pod(age time.Duration, status corev1.PodStatus) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "j-pod", Namespace: "runs", Labels: map[string]string{"job-name": "j"},
		CreationTimestamp: metav1.NewTime(time.Now().Add(-age))}, Status: status}
}

// Status maps Job conditions and stuck pods to what the engine acts on.
func TestKubeStatus(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		objs  []any
		state engine.JobState
		msg   string
	}{
		{"missing", nil, engine.JobMissing, ""},
		{"running", []any{job()}, engine.JobRunning, ""},
		{"complete", []any{job(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})}, engine.JobSucceeded, ""},
		{"false condition is ignored", []any{job(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionFalse})}, engine.JobRunning, ""},
		{"failed with the pod's exit", []any{
			job(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}),
			pod(time.Minute, corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}}),
		}, engine.JobFailed, "BackoffLimitExceeded: Job has reached the specified backoff limit (exit 137, OOMKilled"},
		{"bad image surfaces while running", []any{job(), pod(time.Minute, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "not found"}}}}})},
			engine.JobRunning, "ImagePullBackOff: not found"},
		{"unschedulable after a minute", []any{job(), pod(2*time.Minute, corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/12 nodes are available"}}})},
			engine.JobRunning, "unschedulable: 0/12 nodes are available"},
		{"pending briefly is not reported", []any{job(), pod(10*time.Second, corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/12 nodes are available"}}})},
			engine.JobRunning, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, err := statusKube(c.objs...).Status(ctx, "j")
			if err != nil {
				t.Fatal(err)
			}
			if st.State != c.state || !strings.HasPrefix(st.Message, c.msg) || (c.msg == "" && st.Message != "") {
				t.Fatalf("got %s %q, want %s %q", st.State, st.Message, c.state, c.msg)
			}
		})
	}
}

// Kill deletes the Job, and a Job that is already gone is not an error.
func TestKubeKillIsIdempotent(t *testing.T) {
	k := statusKube(job())
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := k.Kill(ctx, "j"); err != nil {
			t.Fatalf("kill %d: %v", i, err)
		}
	}
	if st, _ := k.Status(ctx, "j"); st.State != engine.JobMissing {
		t.Fatalf("job still there: %s", st.State)
	}
}
