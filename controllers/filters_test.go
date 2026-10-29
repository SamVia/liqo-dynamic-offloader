package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsNamespaceNameAllowed(t *testing.T) {
	r := &LiqoCleanupReconciler{
		ExcludedNamespaces: []string{"kube-*"},
		AllowedNamespaces:  []string{"tenant-*"},
	}

	tests := []struct {
		name      string
		namespace string
		want      bool
	}{
		{"Excluded pattern matches", "kube-system", false},
		{"Allowed pattern matches", "tenant-a", true},
		{"Allowed pattern mismatch", "default", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.isNamespaceNameAllowed(tt.namespace); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsNamespaceLabelsAllowed(t *testing.T) {
	r := &LiqoCleanupReconciler{
		BlacklistLabels: map[string]string{"ignore": "true"},
		WhitelistLabels: map[string]string{"managed-by": "liqo"},
	}

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"Blacklisted label present", map[string]string{"ignore": "true"}, false},
		{"Whitelisted label missing", map[string]string{"other": "value"}, false},
		{"Whitelisted label mismatch", map[string]string{"managed-by": "helm"}, false},
		{"Valid labels", map[string]string{"managed-by": "liqo"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}
			if got := r.isNamespaceLabelsAllowed(ns); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsLiqoRemoteNode(t *testing.T) {
	tests := []struct {
		name string
		node corev1.Node
		want bool
	}{
		{"Has remote label", corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"liqo.io/remote-cluster-id": "cluster-a"}}}, true},
		{"Has virtual prefix", corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "virtual-node-1"}}, true},
		{"Standard node", corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ip-10-0-0-1"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLiqoRemoteNode(tt.node); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
