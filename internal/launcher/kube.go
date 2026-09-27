package launcher

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
)

const (
	LabelManaged = "app.kubernetes.io/managed-by"
	LabelRun     = "ai-flow.dev/run"
	LabelSeq     = "ai-flow.dev/seq"
	LabelNode    = "ai-flow.dev/node"
	LabelRole    = "ai-flow.dev/role"
	LabelEgress  = "ai-flow.dev/egress"
	tokenPath    = "/var/run/secrets/ai-flow"
)

// Kube runs node visits as Jobs in the runs namespace.
type Kube struct {
	cs   kubernetes.Interface
	cfg  *config.Environment
	pull corev1.PullPolicy
}

// Client builds a clientset from in-cluster config, falling back to kubeconfig.
func Client() (kubernetes.Interface, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config or kubeconfig: %w", err)
		}
	}
	return kubernetes.NewForConfig(rc)
}

func NewKube(cs kubernetes.Interface, env *config.Environment) *Kube {
	return &Kube{cs: cs, cfg: env, pull: corev1.PullPolicy(env.Runs.ImagePullPolicy)}
}

func (k *Kube) ns() string { return k.cfg.Runs.Namespace }

func (k *Kube) Launch(ctx context.Context, s engine.LaunchSpec) (string, error) {
	name := JobName(s)
	labels := map[string]string{
		LabelManaged: "ai-flow",
		LabelRun:     s.RunID,
		LabelSeq:     strconv.Itoa(s.Seq),
		LabelNode:    strings.ReplaceAll(s.Node, "_", "-"),
		LabelRole:    "node",
	}
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	if s.Internet {
		podLabels[LabelEgress] = "internet"
	}
	timeout := int64(s.Timeout.Seconds()) + 120
	ttl := int32(k.cfg.Runs.JobTTL.Seconds())
	backoff := int32(s.Retries)
	expiry := int64(3600)
	if int64(s.Timeout.Seconds())+1800 > expiry {
		expiry = int64(s.Timeout.Seconds()) + 1800
	}

	env := []corev1.EnvVar{
		{Name: "AI_FLOW_POD_URL", Value: k.cfg.Server.PodURL},
		{Name: "AI_FLOW_SA_TOKEN", Value: tokenPath + "/token"},
		{Name: "AI_FLOW_WORKDIR", Value: "/work"},
	}
	mounts := []corev1.VolumeMount{
		{Name: "ai-flow-token", MountPath: tokenPath, ReadOnly: true},
		{Name: "work", MountPath: "/work"},
		{Name: "tmp", MountPath: "/tmp"},
	}
	volumes := []corev1.Volume{
		{Name: "ai-flow-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: k.cfg.Runs.TokenAudience, ExpirationSeconds: &expiry, Path: "token",
			}}},
		}}},
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	for i, sec := range s.Secrets {
		if sec.Env != "" {
			env = append(env, corev1.EnvVar{Name: sec.Env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: sec.SecretName}, Key: sec.Key,
			}}})
		}
		if sec.File != "" {
			vol := fmt.Sprintf("secret-%d", i)
			volumes = append(volumes, corev1.Volume{Name: vol, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: sec.SecretName, Items: []corev1.KeyToPath{{Key: sec.Key, Path: "value"}},
			}}})
			mounts = append(mounts, corev1.VolumeMount{Name: vol, MountPath: sec.File, SubPath: "value", ReadOnly: true})
		}
	}

	no, yes := false, true
	uid := int64(1000)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.ns(), Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &timeout,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           k.cfg.Runs.ServiceAccount,
					AutomountServiceAccountToken: &no,
					EnableServiceLinks:           &no,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "node",
						Image:           s.Image,
						ImagePullPolicy: k.pull,
						Command:         []string{"ai-flow", "node"},
						Env:             env,
						VolumeMounts:    mounts,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(k.cfg.Runs.CPU), corev1.ResourceMemory: resource.MustParse(k.cfg.Runs.Memory)},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &no,
							ReadOnlyRootFilesystem:   &yes,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
	_, err := k.cs.BatchV1().Jobs(k.ns()).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return name, nil
	}
	return name, err
}

func (k *Kube) Status(ctx context.Context, name string) (engine.JobStatus, error) {
	job, err := k.cs.BatchV1().Jobs(k.ns()).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return engine.JobStatus{State: engine.JobMissing}, nil
	}
	if err != nil {
		return engine.JobStatus{}, err
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return engine.JobStatus{State: engine.JobSucceeded}, nil
		case batchv1.JobFailed:
			msg := c.Reason
			if c.Message != "" {
				msg += ": " + c.Message
			}
			if detail := k.podFailure(ctx, name); detail != "" {
				msg += " (" + detail + ")"
			}
			return engine.JobStatus{State: engine.JobFailed, Message: msg}, nil
		}
	}
	// Surface pods stuck before start (bad image, unschedulable).
	if detail := k.podWaiting(ctx, name); detail != "" {
		return engine.JobStatus{State: engine.JobRunning, Message: detail}, nil
	}
	return engine.JobStatus{State: engine.JobRunning}, nil
}

func (k *Kube) pods(ctx context.Context, job string) []corev1.Pod {
	list, err := k.cs.CoreV1().Pods(k.ns()).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil {
		return nil
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].CreationTimestamp.Before(&list.Items[j].CreationTimestamp)
	})
	return list.Items
}

func (k *Kube) podFailure(ctx context.Context, job string) string {
	pods := k.pods(ctx, job)
	if len(pods) == 0 {
		return ""
	}
	p := pods[len(pods)-1]
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			return fmt.Sprintf("exit %d, %s %s", t.ExitCode, t.Reason, strings.TrimSpace(t.Message))
		}
	}
	return string(p.Status.Phase)
}

func (k *Kube) podWaiting(ctx context.Context, job string) string {
	for _, p := range k.pods(ctx, job) {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull" || w.Reason == "CreateContainerConfigError") {
				return w.Reason + ": " + w.Message
			}
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && time.Since(p.CreationTimestamp.Time) > time.Minute {
				return "unschedulable: " + c.Message
			}
		}
	}
	return ""
}

func (k *Kube) Kill(ctx context.Context, name string) error {
	bg := metav1.DeletePropagationBackground
	err := k.cs.BatchV1().Jobs(k.ns()).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &bg})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Identify implements broker.PodIdentifier with a TokenReview: the pod's
// projected token proves which pod it is; the pod's labels say which visit.
func (k *Kube) Identify(ctx context.Context, token string) (string, int, error) {
	tr, err := k.cs.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{k.cfg.Runs.TokenAudience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("token review: %w", err)
	}
	if !tr.Status.Authenticated {
		return "", 0, fmt.Errorf("token not authenticated: %s", tr.Status.Error)
	}
	wantUser := fmt.Sprintf("system:serviceaccount:%s:%s", k.ns(), k.cfg.Runs.ServiceAccount)
	if tr.Status.User.Username != wantUser {
		return "", 0, fmt.Errorf("token belongs to %s, not %s", tr.Status.User.Username, wantUser)
	}
	podName := first(tr.Status.User.Extra["authentication.kubernetes.io/pod-name"])
	podUID := first(tr.Status.User.Extra["authentication.kubernetes.io/pod-uid"])
	if podName == "" {
		return "", 0, fmt.Errorf("token is not bound to a pod")
	}
	pod, err := k.cs.CoreV1().Pods(k.ns()).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", 0, err
	}
	if string(pod.UID) != podUID {
		return "", 0, fmt.Errorf("pod uid mismatch")
	}
	run := pod.Labels[LabelRun]
	seq, err := strconv.Atoi(pod.Labels[LabelSeq])
	if run == "" || err != nil {
		return "", 0, fmt.Errorf("pod %s is not an ai-flow node", podName)
	}
	return run, seq, nil
}

func first(xs authv1.ExtraValue) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[0]
}
