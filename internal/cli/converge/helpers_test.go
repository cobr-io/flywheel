package converge

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDeploymentDetail_ProgressingFalseReasonWins(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"replicas": int64(1)},
		"status": map[string]any{
			"availableReplicas": int64(0),
			"conditions": []any{
				map[string]any{
					"type":    "Progressing",
					"status":  "False",
					"reason":  "ImagePullBackOff",
					"message": "Back-off pulling image",
				},
			},
		},
	}}
	if got := deploymentDetail(u); got != "ImagePullBackOff" {
		t.Errorf("deploymentDetail = %q, want ImagePullBackOff", got)
	}
}

func TestDeploymentDetail_AvailableRatioFallback(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"replicas": int64(3)},
		"status": map[string]any{
			"availableReplicas": int64(1),
		},
	}}
	if got := deploymentDetail(u); got != "1/3 available" {
		t.Errorf("deploymentDetail = %q, want '1/3 available'", got)
	}
}
