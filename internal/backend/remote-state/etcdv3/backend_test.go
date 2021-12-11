package etcd

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform/internal/backend"
	etcdv3 "go.etcd.io/etcd/clientv3"
)

var (
	etcdv3Endpoints = strings.Split(os.Getenv("TF_ETCDV3_ENDPOINTS"), ",")
)

const (
	keyPrefix = "tf-unit"
)

func TestBackend_impl(t *testing.T) {
	var _ backend.Backend = new(Backend)
}

func cleanupEtcdv3(t *testing.T) {
	client, err := etcdv3.New(etcdv3.Config{
		Endpoints: etcdv3Endpoints,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := client.KV.Delete(context.TODO(), keyPrefix, etcdv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Cleaned up %d keys.", res.Deleted)
}

func prepareEtcdv3(t *testing.T) {
	skip := os.Getenv("TF_ACC") == "" && os.Getenv("TF_ETCDV3_TEST") == ""
	if skip {
		t.Log("etcd server tests require setting TF_ACC or TF_ETCDV3_TEST")
		t.Skip()
	}
	if reflect.DeepEqual(etcdv3Endpoints, []string{""}) {
		t.Fatal("etcd server tests require setting TF_ETCDV3_ENDPOINTS")
	}
	cleanupEtcdv3(t)
}

func TestBackend(t *testing.T) {
	prepareEtcdv3(t)
	defer cleanupEtcdv3(t)

	prefix := fmt.Sprintf("%s/%s/", keyPrefix, time.Now().Format(time.RFC3339))

	// Get the backend. We need two to test locking.
	b1 := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"endpoints": stringsToInterfaces(etcdv3Endpoints),
		"prefix":    prefix,
	}))

	b2 := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"endpoints": stringsToInterfaces(etcdv3Endpoints),
		"prefix":    prefix,
	}))

	// Test
	backend.TestBackendStates(t, b1)
	backend.TestBackendStateLocks(t, b1, b2)
	backend.TestBackendStateForceUnlock(t, b1, b2)
}

func TestBackend_lockDisabled(t *testing.T) {
	prepareEtcdv3(t)
	defer cleanupEtcdv3(t)

	prefix := fmt.Sprintf("%s/%s/", keyPrefix, time.Now().Format(time.RFC3339))

	// Get the backend. We need two to test locking.
	b1 := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"endpoints": stringsToInterfaces(etcdv3Endpoints),
		"prefix":    prefix,
		"lock":      false,
	}))

	b2 := backend.TestBackendConfig(t, New(), backend.TestWrapConfig(map[string]interface{}{
		"endpoints": stringsToInterfaces(etcdv3Endpoints),
		"prefix":    prefix + "/" + "different", // Diff so locking test would fail if it was locking
		"lock":      false,
	}))

	// Test
	backend.TestBackendStateLocks(t, b1, b2)
}

func stringsToInterfaces(strSlice []string) []interface{} {
	var interfaceSlice []interface{}
	for _, v := range strSlice {
		interfaceSlice = append(interfaceSlice, v)
	}
	return interfaceSlice
}
