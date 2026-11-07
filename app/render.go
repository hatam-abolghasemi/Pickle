package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Files are written the way people write manifests by hand: apiVersion,
// kind, metadata, then the body; name first; within a container image before
// ports before probes; and so on. Keys pickle doesn't know come after the
// known ones, alphabetically. Maps of user-chosen keys (labels, data,
// selectors...) are alphabetical.

// keyOrder ranks well-known keys in any mapping.
var keyOrder = rankOf(
	"apiVersion", "kind", "metadata",
	"name", "generateName", "namespace", "labels", "annotations",
	"type", "immutable",
	"spec", "data", "stringData", "binaryData",
	// workloads
	"replicas", "revisionHistoryLimit", "minReadySeconds", "progressDeadlineSeconds", "strategy", "updateStrategy",
	"podManagementPolicy", "serviceName", "schedule", "timeZone", "concurrencyPolicy", "suspend",
	"successfulJobsHistoryLimit", "failedJobsHistoryLimit", "parallelism", "completions", "backoffLimit",
	"activeDeadlineSeconds", "ttlSecondsAfterFinished", "selector", "jobTemplate", "template", "volumeClaimTemplates",
	// services and ingresses
	"ingressClassName", "clusterIP", "externalName", "loadBalancerIP", "externalTrafficPolicy",
	"defaultBackend", "tls", "hosts", "secretName", "host", "http", "paths", "path", "pathType", "backend", "service",
	// env and selectors
	"key", "operator", "value", "values", "valueFrom", "configMapKeyRef", "secretKeyRef", "fieldRef", "resourceFieldRef",
	// ports
	"port", "containerPort", "targetPort", "nodePort", "protocol", "number",
	"ports",
	// rbac
	"rules", "roleRef", "subjects",
)

// podOrder is for pod specs; containerOrder for containers.
var (
	podOrder = rankOf("serviceAccountName", "automountServiceAccountToken", "imagePullSecrets", "restartPolicy",
		"priorityClassName", "nodeName", "nodeSelector", "affinity", "tolerations", "topologySpreadConstraints",
		"hostNetwork", "dnsPolicy", "dnsConfig", "enableServiceLinks", "terminationGracePeriodSeconds",
		"securityContext", "initContainers", "containers", "volumes")
	containerOrder = rankOf("name", "image", "imagePullPolicy", "command", "args", "workingDir", "ports",
		"envFrom", "env", "resources", "readinessProbe", "livenessProbe", "startupProbe", "lifecycle",
		"securityContext", "volumeMounts", "volumeDevices")
)

// freeForm keys hold maps whose keys are chosen by people, not the API.
var freeForm = map[string]bool{
	"labels": true, "annotations": true, "data": true, "stringData": true, "binaryData": true,
	"matchLabels": true, "nodeSelector": true, "parameters": true, "properties": true,
	"definitions": true, "patternProperties": true, "limits": true, "requests": true,
}

func rankOf(keys ...string) map[string]int {
	r := make(map[string]int, len(keys))
	for i, k := range keys {
		if _, ok := r[k]; !ok {
			r[k] = i
		}
	}
	return r
}

// toYAML renders an object's JSON as YAML in that order.
func toYAML(j []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(j))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	n, err := yamlNode(v, "")
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(n); err != nil {
		return nil, err
	}
	return b.Bytes(), e.Close()
}

func yamlNode(v any, parent string) (*yaml.Node, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := orderedKeys(t, parent)
		n := &yaml.Node{Kind: yaml.MappingNode, Content: make([]*yaml.Node, 0, 2*len(keys))}
		for _, k := range keys {
			vn, err := yamlNode(t[k], k)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, strNode(k), vn)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Content: make([]*yaml.Node, 0, len(t))}
		for _, x := range t {
			xn, err := yamlNode(x, parent)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, xn)
		}
		return n, nil
	case string:
		return strNode(t), nil
	case json.Number:
		if _, err := t.Int64(); err == nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: t.String()}, nil
		}
		f, err := t.Float64()
		if err != nil {
			return nil, err
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(f, 'g', -1, 64)}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(t)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected %T in JSON", v)
}

// strNode is a string scalar. The encoder quotes it when it would read as
// another type; old YAML 1.1 words (yes, on, n...) and sexagesimal numbers are
// quoted too, since kubectl still reads those as booleans and numbers.
func strNode(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	switch {
	case strings.Contains(s, "\n"):
		n.Style = yaml.LiteralStyle
	case yaml11Bools[s] || base60.MatchString(s):
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

var (
	yaml11Bools = map[string]bool{}
	base60      = regexp.MustCompile(`^[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+(?:\.[0-9_]*)?$`)
)

func init() {
	for _, w := range []string{"y", "yes", "n", "no", "true", "false", "on", "off"} {
		for _, v := range []string{w, strings.ToUpper(w), strings.ToUpper(w[:1]) + w[1:]} {
			yaml11Bools[v] = true
		}
	}
}

func orderedKeys(m map[string]any, parent string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	if freeForm[parent] || parent == "selector" && !hasAny(m, "matchLabels", "matchExpressions") {
		sort.Strings(keys)
		return keys
	}
	rank := keyOrder
	switch {
	case hasAny(m, "containers"):
		rank = podOrder
	case hasAny(m, "image"):
		rank = containerOrder
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, iok := rank[keys[i]]
		rj, jok := rank[keys[j]]
		switch {
		case iok && jok:
			return ri < rj
		case iok != jok:
			return iok
		}
		return keys[i] < keys[j]
	})
	return keys
}

func hasAny(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}
