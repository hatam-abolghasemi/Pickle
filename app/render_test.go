package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// TestCleanOutput: objects as the API returns them, defaults and all, are
// written the way they'd be written by hand (testdata/clean/*.yaml).
func TestCleanOutput(t *testing.T) {
	for _, name := range []string{"deployment-api", "service-api"} {
		var u unstructured.Unstructured
		if err := json.Unmarshal(mustRead(t, "testdata/clean/"+name+".json"), &u.Object); err != nil {
			t.Fatal(err)
		}
		got, err := render(readHeader(&u), normalize(&u))
		if err != nil {
			t.Fatal(err)
		}
		if want := string(mustRead(t, "testdata/clean/"+name+".yaml")); string(got) != want {
			t.Errorf("%s:\n%s\nwant:\n%s", name, got, want)
		}
	}
}

func normalized(t *testing.T, y string) map[string]any {
	t.Helper()
	var u unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(y), &u.Object); err != nil {
		t.Fatal(err)
	}
	return normalize(&u).Object
}

func has(m map[string]any, path ...string) bool {
	for i, p := range path {
		v, ok := m[p]
		if !ok {
			return false
		}
		if i < len(path)-1 {
			if m, ok = v.(map[string]any); !ok {
				return false
			}
		}
	}
	return true
}

func TestDropDefaults(t *testing.T) {
	sts := normalized(t, `{apiVersion: apps/v1, kind: StatefulSet, metadata: {name: db},
	  spec: {replicas: 3, serviceName: db, podManagementPolicy: OrderedReady, revisionHistoryLimit: 10,
	    updateStrategy: {type: RollingUpdate, rollingUpdate: {partition: 0}},
	    persistentVolumeClaimRetentionPolicy: {whenDeleted: Retain, whenScaled: Retain},
	    volumeClaimTemplates: [{apiVersion: v1, kind: PersistentVolumeClaim, metadata: {name: data},
	      spec: {accessModes: [ReadWriteOnce], volumeMode: Filesystem, resources: {requests: {storage: 1Gi}}}, status: {phase: Pending}}],
	    template: {metadata: {labels: {app: db}}, spec: {containers: [{name: db, image: "postgres:16", imagePullPolicy: Always}]}}}}`)
	for _, p := range [][]string{{"spec", "podManagementPolicy"}, {"spec", "revisionHistoryLimit"}, {"spec", "updateStrategy"}, {"spec", "persistentVolumeClaimRetentionPolicy"}} {
		if has(sts, p...) {
			t.Errorf("StatefulSet %v is the default and must go", p)
		}
	}
	vct := sts["spec"].(map[string]any)["volumeClaimTemplates"].([]any)[0].(map[string]any)
	if has(vct, "status") || has(vct, "kind") || has(vct, "spec", "volumeMode") || !has(vct, "spec", "accessModes") {
		t.Errorf("volumeClaimTemplate: %v", vct)
	}
	c := sts["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if c["imagePullPolicy"] != "Always" {
		t.Error("imagePullPolicy: Always is not the default for a tagged image and must stay")
	}

	cj := normalized(t, `{apiVersion: batch/v1, kind: CronJob, metadata: {name: c},
	  spec: {schedule: "0 * * * *", concurrencyPolicy: Forbid, suspend: false, successfulJobsHistoryLimit: 3, failedJobsHistoryLimit: 1,
	    jobTemplate: {metadata: {creationTimestamp: null}, spec: {backoffLimit: 6, completionMode: NonIndexed, template: {spec: {restartPolicy: OnFailure,
	      containers: [{name: c, image: busybox, imagePullPolicy: Always, terminationMessagePolicy: File}]}}}}}}`)
	spec := cj["spec"].(map[string]any)
	if spec["concurrencyPolicy"] != "Forbid" || has(cj, "spec", "suspend") || has(cj, "spec", "successfulJobsHistoryLimit") || has(cj, "spec", "jobTemplate", "metadata") {
		t.Errorf("CronJob spec: %v", spec)
	}
	ps := spec["jobTemplate"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	cc := ps["containers"].([]any)[0].(map[string]any)
	if ps["restartPolicy"] != "OnFailure" || has(cc, "imagePullPolicy") || has(cc, "terminationMessagePolicy") {
		t.Errorf("job pod: %v", ps)
	}

	ns := normalized(t, `{apiVersion: v1, kind: Namespace, metadata: {name: payments, labels: {kubernetes.io/metadata.name: payments, team: pay}}, spec: {finalizers: [kubernetes]}}`)
	if has(ns, "spec") || has(ns, "metadata", "labels", "kubernetes.io/metadata.name") || !has(ns, "metadata", "labels", "team") {
		t.Errorf("Namespace: %v", ns)
	}

	svc := normalized(t, `{apiVersion: v1, kind: Service, metadata: {name: s}, spec: {type: NodePort, externalTrafficPolicy: Local,
	  ports: [{port: 80, targetPort: 80, nodePort: 30080, protocol: UDP}, {port: 443, targetPort: 8443}]}}`)
	ports := svc["spec"].(map[string]any)["ports"].([]any)
	if p := ports[0].(map[string]any); has(p, "targetPort") || p["protocol"] != "UDP" || p["nodePort"] == nil {
		t.Errorf("port 0: %v", p)
	}
	if p := ports[1].(map[string]any); !has(p, "targetPort") {
		t.Errorf("a targetPort other than port must stay: %v", p)
	}
	if svc["spec"].(map[string]any)["externalTrafficPolicy"] != "Local" {
		t.Error("externalTrafficPolicy: Local must stay")
	}

	cr := normalized(t, `{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, metadata: {name: admin}, aggregationRule: {clusterRoleSelectors: [{matchLabels: {a: b}}]}, rules: [{verbs: [get]}]}`)
	if has(cr, "rules") || !has(cr, "aggregationRule") {
		t.Errorf("aggregated ClusterRole: %v", cr)
	}
}

func TestDefaultPullPolicy(t *testing.T) {
	for image, want := range map[string]string{
		"nginx": "Always", "nginx:latest": "Always", "nginx:1.27": "IfNotPresent",
		"registry:5000/team/app": "Always", "registry:5000/team/app:v1": "IfNotPresent",
		"nginx@sha256:abc": "IfNotPresent",
	} {
		if got := defaultPullPolicy(image); got != want {
			t.Errorf("%s: %s, want %s", image, got, want)
		}
	}
}

// TestOrder: names first, and the order people write.
func TestOrder(t *testing.T) {
	got, err := render(header{}, &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"rules": []any{}, "ingressClassName": "nginx"}, "metadata": map[string]any{"annotations": map[string]any{"b": "1", "a": "2"}, "name": "web", "namespace": "shop"},
		"kind": "Ingress", "apiVersion": "networking.k8s.io/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: web\n  namespace: shop\n  annotations:\n    a: \"2\"\n    b: \"1\"\nspec:\n  ingressClassName: nginx\n  rules: []\n"
	if !strings.Contains(string(got), want) {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func mustRead(t testing.TB, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
