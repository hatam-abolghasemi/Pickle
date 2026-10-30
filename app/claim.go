package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// A <cluster>/ directory belongs to one Kubernetes cluster, recorded in
// <cluster>/_cluster-id.txt as the UID of that cluster's kube-system
// namespace. Two pickles given the same PICKLE_CLUSTER would otherwise mix
// their objects in one directory, and each restart would delete the other
// cluster's files as stale and re-add its own. The first to push its claim
// keeps the directory; any other refuses to start, or stops as soon as it
// sees the claim, without pushing.

var errClusterClaimed = errors.New("directory belongs to another cluster")

type claimError struct {
	dir, have, want string
}

func (c *claimError) Error() string {
	return fmt.Sprintf("%s/ in the repo mirrors another cluster (kube-system UID %s), not this one (%s): every cluster needs its own PICKLE_CLUSTER",
		c.dir, c.have, c.want)
}

func (c *claimError) Is(target error) bool { return target == errClusterClaimed }

var namespacesGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// clusterUID identifies the cluster pickle runs against.
func clusterUID(ctx context.Context, dyn dynamic.Interface) (string, error) {
	ns, err := dyn.Resource(namespacesGVR).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read the kube-system namespace to identify this cluster: %w", err)
	}
	uid := string(ns.GetUID())
	if uid == "" {
		return "", errors.New("the kube-system namespace has no UID")
	}
	return uid, nil
}

func renderClaim(uid string) []byte {
	return []byte("# The cluster this directory mirrors: the UID of its kube-system namespace. pickle refuses to write here from any other cluster.\n" + uid + "\n")
}

// parseClaim returns the UID in a _cluster-id.txt, or "" if it holds none.
func parseClaim(b []byte) string {
	s := bufio.NewScanner(bytes.NewReader(b))
	uid := ""
	for s.Scan() {
		if l := bytes.TrimSpace(s.Bytes()); len(l) > 0 && l[0] != '#' {
			uid = string(l)
		}
	}
	return uid
}

// aborted returns why ctx was cancelled if pickle has to stop with an error,
// nil for an ordinary shutdown.
func aborted(ctx context.Context) error {
	if c := context.Cause(ctx); errors.Is(c, errClusterClaimed) {
		return c
	}
	return nil
}
