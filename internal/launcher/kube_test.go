package launcher

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
)

func TestTmpDir(t *testing.T) {
	if d := tmpDir(""); d.Medium != "" || d.SizeLimit != nil {
		t.Fatalf("default /tmp must stay on disk: %+v", d)
	}
	if d := tmpDir("1Gi"); d.Medium != corev1.StorageMediumMemory || d.SizeLimit.String() != "1Gi" {
		t.Fatalf("tmpSize: %+v", d)
	}
}

// Evictions do not use up a step's retries, and the environment can label
// node pods (a descheduler exclusion) without overriding ai-flow's labels.
func TestJobSurvivesEvictionAndCarriesPodLabels(t *testing.T) {
	cs := fake.NewSimpleClientset()
	env := &config.Environment{Runs: config.Runs{Namespace: "runs", CPU: "1", Memory: "1Gi", CPURequest: "100m", MemoryRequest: "256Mi",
		PodLabels:      map[string]string{"descheduler.example/exclude": "true", LabelRun: "spoofed"},
		PodAnnotations: map[string]string{"note": "x"}}}
	k := NewKube(cs, env)
	name, err := k.Launch(context.Background(), engine.LaunchSpec{RunID: "r-1", Seq: 3, Node: "browser_qa", Type: "agent", Image: "img", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	job, err := cs.BatchV1().Jobs("runs").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template
	if pod.Labels["descheduler.example/exclude"] != "true" || pod.Labels[LabelRun] != "r-1" || pod.Annotations["note"] != "x" {
		t.Fatalf("pod labels %v annotations %v", pod.Labels, pod.Annotations)
	}
	p := job.Spec.PodFailurePolicy
	if p == nil || len(p.Rules) != 1 || p.Rules[0].Action != "Ignore" || p.Rules[0].OnPodConditions[0].Type != corev1.DisruptionTarget {
		t.Fatalf("pod failure policy: %+v", p)
	}
}
