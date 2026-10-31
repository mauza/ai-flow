package launcher

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestTmpDir(t *testing.T) {
	if d := tmpDir(""); d.Medium != "" || d.SizeLimit != nil {
		t.Fatalf("default /tmp must stay on disk: %+v", d)
	}
	if d := tmpDir("1Gi"); d.Medium != corev1.StorageMediumMemory || d.SizeLimit.String() != "1Gi" {
		t.Fatalf("tmpSize: %+v", d)
	}
}
