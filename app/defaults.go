package main

import (
	"reflect"
	"strings"
)

// dropDefaults removes what the API server fills in when a field is left
// out, but only where the value is exactly that default: applying the file
// gives back the same object. Nulls go everywhere, since null means unset.
func dropDefaults(obj map[string]any) {
	dropNulls(obj)
	group, _, _ := strings.Cut(str(obj, "apiVersion"), "/")
	if !strings.Contains(str(obj, "apiVersion"), "/") {
		group = ""
	}
	spec, _ := obj["spec"].(map[string]any)

	switch group + "/" + str(obj, "kind") {
	case "apps/Deployment":
		dropEq(spec, "progressDeadlineSeconds", 600)
		dropEq(spec, "revisionHistoryLimit", 10)
		dropEq(spec, "strategy", map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": "25%", "maxUnavailable": "25%"}})
		podTemplate(spec)
	case "apps/StatefulSet":
		dropEq(spec, "revisionHistoryLimit", 10)
		dropEq(spec, "podManagementPolicy", "OrderedReady")
		dropEq(spec, "updateStrategy", map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"partition": 0}})
		dropEq(spec, "persistentVolumeClaimRetentionPolicy", map[string]any{"whenDeleted": "Retain", "whenScaled": "Retain"})
		for _, c := range listOfMaps(spec, "volumeClaimTemplates") {
			delete(c, "status")
			if c["apiVersion"] == "v1" && c["kind"] == "PersistentVolumeClaim" {
				delete(c, "apiVersion")
				delete(c, "kind")
			}
			cs, _ := c["spec"].(map[string]any)
			dropEq(cs, "volumeMode", "Filesystem")
		}
		podTemplate(spec)
	case "apps/DaemonSet":
		dropEq(spec, "revisionHistoryLimit", 10)
		dropEq(spec, "updateStrategy", map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 0, "maxUnavailable": 1}})
		podTemplate(spec)
	case "apps/ReplicaSet":
		podTemplate(spec)
	case "batch/CronJob":
		dropEq(spec, "concurrencyPolicy", "Allow")
		dropEq(spec, "suspend", false)
		dropEq(spec, "successfulJobsHistoryLimit", 3)
		dropEq(spec, "failedJobsHistoryLimit", 1)
		jt, _ := spec["jobTemplate"].(map[string]any)
		js, _ := jt["spec"].(map[string]any)
		jobSpec(js)
		dropEmpty(jt, "metadata")
	case "batch/Job":
		jobSpec(spec)
	case "/Pod":
		podSpec(spec)
	case "/PodTemplate":
		t, _ := obj["template"].(map[string]any)
		ts, _ := t["spec"].(map[string]any)
		podSpec(ts)
	case "/Service":
		dropEq(spec, "sessionAffinity", "None")
		dropEq(spec, "internalTrafficPolicy", "Cluster")
		if spec["ipFamilyPolicy"] == "SingleStack" {
			delete(spec, "ipFamilyPolicy")
			delete(spec, "ipFamilies") // the cluster's own family
		}
		if t := str(spec, "type"); t == "NodePort" || t == "LoadBalancer" {
			dropEq(spec, "externalTrafficPolicy", "Cluster")
		}
		if spec["type"] == "LoadBalancer" {
			dropEq(spec, "allocateLoadBalancerNodePorts", true)
		}
		for _, p := range listOfMaps(spec, "ports") {
			dropEq(p, "protocol", "TCP")
			if num(p["targetPort"]) == num(p["port"]) && num(p["port"]) != 0 {
				delete(p, "targetPort")
			}
		}
	case "/PersistentVolumeClaim":
		dropEq(spec, "volumeMode", "Filesystem")
	case "/Namespace":
		dropEq(obj, "spec", map[string]any{"finalizers": []any{"kubernetes"}})
		md, _ := obj["metadata"].(map[string]any)
		if l, _ := md["labels"].(map[string]any); l != nil && l["kubernetes.io/metadata.name"] == md["name"] {
			delete(l, "kubernetes.io/metadata.name")
			dropEmpty(md, "labels")
		}
	case "rbac.authorization.k8s.io/ClusterRole":
		if _, ok := obj["aggregationRule"]; ok {
			delete(obj, "rules") // filled in by the aggregation controller
		}
	case "storage.k8s.io/StorageClass":
		dropEq(obj, "reclaimPolicy", "Delete")
		dropEq(obj, "volumeBindingMode", "Immediate")
	}
}

func jobSpec(spec map[string]any) {
	dropEq(spec, "backoffLimit", 6)
	dropEq(spec, "completionMode", "NonIndexed")
	dropEq(spec, "completions", 1)
	dropEq(spec, "parallelism", 1)
	dropEq(spec, "suspend", false)
	dropEq(spec, "manualSelector", false)
	dropEq(spec, "podReplacementPolicy", "TerminatingOrFailed")
	podTemplate(spec)
}

func podTemplate(spec map[string]any) {
	t, _ := spec["template"].(map[string]any)
	if t == nil {
		return
	}
	dropEmpty(t, "metadata")
	ts, _ := t["spec"].(map[string]any)
	podSpec(ts)
}

func podSpec(spec map[string]any) {
	if spec == nil {
		return
	}
	dropEq(spec, "dnsPolicy", "ClusterFirst")
	dropEq(spec, "restartPolicy", "Always")
	dropEq(spec, "schedulerName", "default-scheduler")
	dropEq(spec, "terminationGracePeriodSeconds", 30)
	dropEq(spec, "enableServiceLinks", true)
	dropEmpty(spec, "securityContext")
	if spec["serviceAccount"] == spec["serviceAccountName"] {
		delete(spec, "serviceAccount") // deprecated copy of serviceAccountName
	}
	for _, key := range []string{"initContainers", "containers"} {
		for _, c := range listOfMaps(spec, key) {
			containerDefaults(c)
		}
	}
	for _, v := range listOfMaps(spec, "volumes") {
		for _, src := range []string{"configMap", "secret", "projected", "downwardAPI"} {
			s, _ := v[src].(map[string]any)
			dropEq(s, "defaultMode", 420) // 0644
			for _, item := range listOfMaps(s, "items") {
				fieldRef(item)
			}
			for _, ps := range listOfMaps(s, "sources") {
				da, _ := ps["downwardAPI"].(map[string]any)
				for _, item := range listOfMaps(da, "items") {
					fieldRef(item)
				}
			}
		}
		hp, _ := v["hostPath"].(map[string]any)
		dropEq(hp, "type", "")
	}
}

func containerDefaults(c map[string]any) {
	dropEq(c, "terminationMessagePath", "/dev/termination-log")
	dropEq(c, "terminationMessagePolicy", "File")
	dropEq(c, "imagePullPolicy", defaultPullPolicy(str(c, "image")))
	dropEmpty(c, "resources")
	for _, p := range listOfMaps(c, "ports") {
		dropEq(p, "protocol", "TCP")
	}
	for _, e := range listOfMaps(c, "env") {
		vf, _ := e["valueFrom"].(map[string]any)
		fieldRef(vf)
		rf, _ := vf["resourceFieldRef"].(map[string]any)
		dropEq(rf, "divisor", "0")
	}
	for _, probe := range []string{"readinessProbe", "livenessProbe", "startupProbe"} {
		p, _ := c[probe].(map[string]any)
		if p == nil {
			continue
		}
		dropEq(p, "timeoutSeconds", 1)
		dropEq(p, "periodSeconds", 10)
		dropEq(p, "successThreshold", 1)
		dropEq(p, "failureThreshold", 3)
		hg, _ := p["httpGet"].(map[string]any)
		dropEq(hg, "scheme", "HTTP")
	}
}

func fieldRef(m map[string]any) {
	fr, _ := m["fieldRef"].(map[string]any)
	dropEq(fr, "apiVersion", "v1")
}

// defaultPullPolicy is the API's: Always for :latest or no tag, otherwise
// IfNotPresent.
func defaultPullPolicy(image string) string {
	if strings.Contains(image, "@") {
		return "IfNotPresent"
	}
	name := image[strings.LastIndex(image, "/")+1:]
	if i := strings.LastIndex(name, ":"); i < 0 || name[i+1:] == "latest" {
		return "Always"
	}
	return "IfNotPresent"
}

func dropEq(m map[string]any, key string, def any) {
	if m == nil {
		return
	}
	if v, ok := m[key]; ok && sameJSON(v, def) {
		delete(m, key)
	}
}

func dropEmpty(m map[string]any, key string) {
	if m == nil {
		return
	}
	if v, ok := m[key].(map[string]any); ok && len(v) == 0 {
		delete(m, key)
	}
}

// sameJSON compares a decoded JSON value with a default written in Go:
// numbers by value, whatever their Go type.
func sameJSON(v, def any) bool {
	switch d := def.(type) {
	case int:
		f, ok := toFloat(v)
		return ok && f == float64(d)
	case map[string]any:
		m, ok := v.(map[string]any)
		if !ok || len(m) != len(d) {
			return false
		}
		for k, dv := range d {
			if !sameJSON(m[k], dv) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(v, def)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func num(v any) float64 {
	f, _ := toFloat(v)
	return f
}

func listOfMaps(m map[string]any, key string) []map[string]any {
	if m == nil {
		return nil
	}
	raw, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		if mm, ok := x.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

func dropNulls(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if x == nil {
				delete(t, k)
				continue
			}
			dropNulls(x)
		}
	case []any:
		for _, x := range t {
			dropNulls(x)
		}
	}
}
