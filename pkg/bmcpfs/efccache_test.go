//go:build !windows

/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bmcpfs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kubernetes-sigs/alibaba-cloud-csi-driver/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8smount "k8s.io/mount-utils"
)

type fakeEFCCacheGetter struct {
	object *unstructured.Unstructured
	err    error
	calls  int
}

func (g *fakeEFCCacheGetter) Get(context.Context, string, string) (*unstructured.Unstructured, error) {
	g.calls++
	return g.object, g.err
}

const testFilesystemID = "cpfs-0123abcd"

func validEFCCache() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "efc.aliyun.com/v1alpha1",
		"kind":       "EFCCache",
		"metadata": map[string]any{
			"name":       "training",
			"namespace":  "default",
			"generation": int64(3),
		},
		"spec": map[string]any{
			"sources": []any{map[string]any{
				"name": "data", "source": "cpfs-vpc://cpfs-0123abcd-vpc-000001.cn-hangzhou.cpfs.aliyuncs.com/data",
			}},
			"cache": map[string]any{
				"options": map[string]any{"netType": "tcp"},
			},
		},
		"status": map[string]any{
			"phase":              "Bound",
			"observedGeneration": int64(3),
			"cacheID":            "training-uid",
			"serviceName":        "efc-cache-training",
			"sources": []any{map[string]any{
				"name": "data", "rootPort": int64(17980),
			}},
		},
	}}
}

func TestParseEFCCacheReference(t *testing.T) {
	ref, err := parseEFCCacheReference("tenant-a/cache-a/source-a")
	require.NoError(t, err)
	assert.Equal(t, efcCacheReference{namespace: "tenant-a", cache: "cache-a", source: "source-a"}, ref)

	for _, invalid := range []string{
		"", "cache/source", "namespace/cache/source/extra", "/cache/source",
		"Namespace/cache/source", "namespace/cache/source.with-dot",
	} {
		t.Run(invalid, func(t *testing.T) {
			_, err := parseEFCCacheReference(invalid)
			require.Error(t, err)
		})
	}
}

func TestValidateEFCCache(t *testing.T) {
	ref := efcCacheReference{namespace: "default", cache: "training", source: "data"}
	config, err := validateEFCCache(validEFCCache(), ref, testFilesystemID)
	require.NoError(t, err)
	assert.Equal(t, "efc-cache-training.default.svc.cluster.local", config.domain)
	assert.EqualValues(t, 17980, config.rootPort)
	assert.Equal(t, "training-uid", config.cacheID)
	assert.Equal(t, "tcp", config.netType)

	withoutNetType := validEFCCache()
	require.NoError(t, unstructured.SetNestedField(withoutNetType.Object, map[string]any{}, "spec", "cache", "options"))
	config, err = validateEFCCache(withoutNetType, ref, testFilesystemID)
	require.NoError(t, err)
	assert.Equal(t, "tcp", config.netType)

	povSource := validEFCCache()
	require.NoError(t, unstructured.SetNestedSlice(povSource.Object, []any{map[string]any{
		"name": "data", "source": "cpfs-pov://cpfs-0123abcd-vsc-000001.cn-hangzhou.cpfs.aliyuncs.com/other",
	}}, "spec", "sources"))
	config, err = validateEFCCache(povSource, ref, testFilesystemID)
	require.NoError(t, err)
	assert.Equal(t, "cpfs-pov", config.sourceScheme)
}

func TestValidateEFCCacheRejectsInvalidObjects(t *testing.T) {
	ref := efcCacheReference{namespace: "default", cache: "training", source: "data"}
	tests := []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		match  string
	}{
		{
			name: "deleting",
			mutate: func(object *unstructured.Unstructured) {
				now := metav1.NewTime(time.Now())
				object.SetDeletionTimestamp(&now)
			},
			match: "being deleted",
		},
		{
			name: "stale status",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedField(object.Object, int64(2), "status", "observedGeneration"))
			},
			match: "observedGeneration",
		},
		{
			name: "missing phase",
			mutate: func(object *unstructured.Unstructured) {
				unstructured.RemoveNestedField(object.Object, "status", "phase")
			},
			match: "status.phase is required",
		},
		{
			name: "failed cache",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedField(object.Object, "Failed", "status", "phase"))
			},
			match: "status.phase is Failed",
		},
		{
			name: "missing cache ID",
			mutate: func(object *unstructured.Unstructured) {
				unstructured.RemoveNestedField(object.Object, "status", "cacheID")
			},
			match: "status.cacheID",
		},
		{
			name: "invalid cache ID",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedField(object.Object, "cache,id", "status", "cacheID"))
			},
			match: "status.cacheID",
		},
		{
			name: "missing service",
			mutate: func(object *unstructured.Unstructured) {
				unstructured.RemoveNestedField(object.Object, "status", "serviceName")
			},
			match: "status.serviceName",
		},
		{
			name: "invalid service",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedField(object.Object, "cache,name", "status", "serviceName"))
			},
			match: "status.serviceName",
		},
		{
			name: "missing status source",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedSlice(object.Object, []any{}, "status", "sources"))
			},
			match: "status.sources has no source",
		},
		{
			name: "bad root port",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedSlice(object.Object, []any{map[string]any{"name": "data", "rootPort": int64(65536)}}, "status", "sources"))
			},
			match: "rootPort",
		},
		{
			name: "missing spec source",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedSlice(object.Object, []any{}, "spec", "sources"))
			},
			match: "spec.sources has no source",
		},
		{
			name: "unsupported source scheme",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedSlice(object.Object, []any{map[string]any{"name": "data", "source": "nas-efc://10.0.0.1/data"}}, "spec", "sources"))
			},
			match: "scheme must be",
		},
		{
			name: "different filesystem",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedSlice(object.Object, []any{map[string]any{"name": "data", "source": "cpfs-vpc://cpfs-9999ffff-vpc-000001.cn-hangzhou.cpfs.aliyuncs.com/data"}}, "spec", "sources"))
			},
			match: "does not match volume filesystem",
		},
		{
			name: "mount target without filesystem ID",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedSlice(object.Object, []any{map[string]any{"name": "data", "source": "cpfs-vpc://10.0.0.1/data"}}, "spec", "sources"))
			},
			match: "not a CPFS mount target",
		},
		{
			name: "unsupported cache network",
			mutate: func(object *unstructured.Unstructured) {
				require.NoError(t, unstructured.SetNestedField(object.Object, "roce", "spec", "cache", "options", "netType"))
			},
			match: "unsupported spec.cache.options.netType",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := validEFCCache()
			tt.mutate(object)
			_, err := validateEFCCache(object, ref, testFilesystemID)
			require.ErrorContains(t, err, tt.match)
			assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		})
	}
}

func TestCPFSFilesystemID(t *testing.T) {
	for host, expected := range map[string]string{
		"cpfs-00012een3ng3uxhqlg7io-vpc-rs4set.cn-hangzhou.cpfs.aliyuncs.com": "cpfs-00012een3ng3uxhqlg7io",
		"bmcpfs-37012i9aty1zmsd5a6pvc-009000.cn-wulanchabu.cpfs.aliyuncs.com": "bmcpfs-37012i9aty1zmsd5a6pvc",
		"cpfs-290113jifsm2nj4dg3rrx.cn-wulanchabu.cpfs.aliyuncs.com":          "cpfs-290113jifsm2nj4dg3rrx",
		"CPFS-0123ABCD-VSC-000001.cn-hangzhou.cpfs.aliyuncs.com":              "cpfs-0123abcd",
	} {
		id, ok := cpfsFilesystemID(host)
		assert.True(t, ok, host)
		assert.Equal(t, expected, id, host)
	}
	for _, host := range []string{"10.0.0.1", "cpfs.cn-hangzhou.cpfs.aliyuncs.com", "0d2ab4b84c-xyz12.cn-hangzhou.nas.aliyuncs.com"} {
		_, ok := cpfsFilesystemID(host)
		assert.False(t, ok, host)
	}
}

func TestNodePublishVolumeUsesCacheOnAnotherMountTarget(t *testing.T) {
	povCache := validEFCCache()
	require.NoError(t, unstructured.SetNestedSlice(povCache.Object, []any{map[string]any{
		"name": "data", "source": "cpfs-pov://cpfs-0123abcd-vsc-000001.cn-hangzhou.cpfs.aliyuncs.com/train",
	}}, "spec", "sources"))
	for name, test := range map[string]struct {
		volumeID string
		code     codes.Code
	}{
		"same filesystem over VPC": {volumeID: testFilesystemID + "+fset-a"},
		"different filesystem":     {volumeID: "cpfs-9999ffff", code: codes.FailedPrecondition},
	} {
		t.Run(name, func(t *testing.T) {
			ns := &nodeServer{
				mounter:     k8smount.NewFakeMounter(nil),
				locks:       utils.NewVolumeLocks(),
				credsRoot:   t.TempDir(),
				cacheGetter: &fakeEFCCacheGetter{object: povCache},
			}
			req := publishReq(test.volumeID, t.TempDir()+"/target", map[string]string{
				efcCacheRefAttribute: "default/training/data",
			}, nil, nil)
			_, err := ns.NodePublishVolume(t.Context(), req)
			assert.Equal(t, test.code, status.Code(err))
		})
	}
}

func TestResolveEFCCacheMissingAPIIsRetryable(t *testing.T) {
	getter := &fakeEFCCacheGetter{err: apierrors.NewNotFound(
		schema.GroupResource{Group: efcCacheGVR.Group, Resource: efcCacheGVR.Resource}, "training")}
	ns := &nodeServer{cacheGetter: getter}
	_, err := ns.resolveEFCCache(t.Context(), "default/training/data", testFilesystemID)
	require.ErrorContains(t, err, "ensure the EFCCache CRD and object exist")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	getter.err = apierrors.NewForbidden(
		schema.GroupResource{Group: efcCacheGVR.Group, Resource: efcCacheGVR.Resource}, "training", errors.New("forbidden"))
	_, err = ns.resolveEFCCache(t.Context(), "default/training/data", testFilesystemID)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))

	getter.err = errors.New("API connection refused")
	_, err = ns.resolveEFCCache(t.Context(), "default/training/data", testFilesystemID)
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func TestCacheMountOptionsNetworks(t *testing.T) {
	tests := []struct {
		netType string
		dadi    string
		busy    string
	}{
		{netType: "tcp", dadi: "g_tier_DadiNetType=0"},
		{netType: "erdma", dadi: "g_tier_DadiNetType=2"},
		{netType: "mlx", dadi: "g_tier_DadiNetType=1", busy: "g_tier_EnableBusyRDMA=false"},
		{netType: "eic", dadi: "g_tier_DadiNetType=1", busy: "g_tier_EnableBusyRDMA=true"},
	}
	for _, tt := range tests {
		t.Run(tt.netType, func(t *testing.T) {
			options := cacheMountOptions(efcCacheMountConfig{
				domain: "cache.ns.svc.cluster.local", rootPort: 17980,
				cacheID: "cache-id", netType: tt.netType,
			})
			expected := []string{
				"g_tier_EnableClusterCache=true",
				"g_tier_DadiIsDistributed=true",
				"g_tier_DadiRootClientType=2",
				"g_distributed_RootClientType=2",
				"g_distributed_ReliableRootlistEnable=true",
				"g_distributed_NodeListFromCacheEnable=true",
				"g_tier_DadiUseHashRingRootlist=true",
				"g_tier_EnableSharedCache=false",
				"g_tier_DadiDomainName=cache.ns.svc.cluster.local",
				"g_tier_DadiDomainPort=17980",
				"g_server_CacheId=cache-id",
				tt.dadi,
			}
			if tt.busy != "" {
				expected = append(expected, tt.busy)
			}
			expected = append(expected, "g_tier_DadiDiskAio=true", "g_cpfs_SizeTrusted=true")
			assert.Equal(t, expected, options)
		})
	}
}

func TestCacheMountOptionsWithPublishedEFC(t *testing.T) {
	image := os.Getenv("EFC_SMOKE_IMAGE")
	if image == "" {
		t.Skip("set EFC_SMOKE_IMAGE to verify cache options against an EFC image")
	}
	options := cacheMountOptions(efcCacheMountConfig{
		domain: "cache.default.svc.cluster.local", rootPort: 17980,
		cacheID: "cache-id", netType: "tcp", sourceScheme: "cpfs-vpc",
	})
	serialized, err := json.Marshal(options)
	require.NoError(t, err)
	script := `import json, os, pathlib, runpy, shutil
helper = runpy.run_path(shutil.which('mount.alinas'))
flags = dict(option.split('=', 1) for option in json.loads(os.environ['CACHE_MOUNT_OPTIONS']))
flags['node_id'] = 'smoke-node'
_, arguments = helper['serialize_unas_options'](flags, 'smoke-uuid')
assert '--tier_DadiRootClientType=2' in arguments
assert '--distributed_RootClientType=2' in arguments
assert '--distributed_ReliableRootlistEnable=true' in arguments
assert '--distributed_NodeListFromCacheEnable=true' in arguments
assert '--tier_DadiUseHashRingRootlist=true' in arguments
binary = pathlib.Path(shutil.which('aliyun-alinas-efc')).read_bytes()
assert b'GetNodeList' in binary
for name in flags:
    if name.startswith('g_'):
        assert name[2:].encode() in binary, name
print('Helper serializes discovery flags; EFC contains GetNodeList and the required flag names')
`
	command := exec.Command("docker", "run", "--rm", "-i", "--read-only", "--network=none", "--cap-drop=ALL", "--env", "CACHE_MOUNT_OPTIONS="+string(serialized), "--entrypoint", "python3", image, "-B", "-")
	command.Stdin = strings.NewReader(script)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func containsOptionKey(options []string, key string) bool {
	for _, option := range normalizedMountFlags(options) {
		if option == key || strings.HasPrefix(option, key+"=") {
			return true
		}
	}
	return false
}

func TestValidateCacheManagedMountOptions(t *testing.T) {
	for key := range cacheManagedMountOptionKeys {
		t.Run(key, func(t *testing.T) {
			err := validateCacheManagedMountOptions([]string{"ordinary=true," + key + "=value"})
			require.ErrorContains(t, err, key)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
	err := validateCacheManagedMountOptions([]string{"-o g_tier_EnableClusterCache=true"})
	require.ErrorContains(t, err, "g_tier_EnableClusterCache")
	require.NoError(t, validateCacheManagedMountOptions([]string{"g_tier_Other=true", "cache_hint=bindtag"}))
}

func TestCacheBindTagStableSharingAndIsolation(t *testing.T) {
	config := efcCacheMountConfig{
		domain: "cache.default.svc.cluster.local", rootPort: 17980,
		cacheID: "cache-id", netType: "tcp", sourceScheme: "cpfs-vpc",
	}
	first, err := cacheBindTag("10.0.0.1", networkTypeVPC, config,
		[]string{"b=2,a=1"}, []string{"accesspoint=ap-a"})
	require.NoError(t, err)
	second, err := cacheBindTag("10.0.0.1", networkTypeVPC, config,
		[]string{"a=1", "b=2"}, []string{"accesspoint=ap-a"})
	require.NoError(t, err)
	assert.Equal(t, first, second, "normalized flags and compatible subpaths share a root")
	assert.Regexp(t, `^bmcpfs-efc-cache-v1-[0-9a-f]{20}$`, first)
	assert.NotEqual(t, "bmcpfs-efc-cache-v1-86450c5b4f8fd55aee83", first, "node-list discovery must not reuse the previous DNS-only bindroot")

	differentTarget, _ := cacheBindTag("10.0.0.2", networkTypeVPC, config, []string{"a=1", "b=2"}, []string{"accesspoint=ap-a"})
	differentCache := config
	differentCache.cacheID = "other-id"
	differentCacheTag, _ := cacheBindTag("10.0.0.1", networkTypeVPC, differentCache, []string{"a=1", "b=2"}, []string{"accesspoint=ap-a"})
	differentAuth, _ := cacheBindTag("10.0.0.1", networkTypeVPC, config, []string{"a=1", "b=2"}, []string{"g_unas_STSFile=/run/volume-b/sts.json"})
	authenticatedA, _ := cacheBindTag("10.0.0.1", networkTypeVPC, config, nil, []string{"g_unas_STSFile=/run/volume-a/sts.json"})
	authenticatedB, _ := cacheBindTag("10.0.0.1", networkTypeVPC, config, nil, []string{"g_unas_STSFile=/run/volume-b/sts.json"})
	assert.NotEqual(t, first, differentTarget)
	assert.NotEqual(t, first, differentCacheTag)
	assert.NotEqual(t, first, differentAuth)
	assert.NotEqual(t, authenticatedA, authenticatedB, "volume-specific credential paths isolate authenticated roots")
}

func TestNodePublishVolumeDirectDoesNotInitializeCacheClient(t *testing.T) {
	factoryCalls := 0
	ns := &nodeServer{
		mounter:   k8smount.NewFakeMounter(nil),
		locks:     utils.NewVolumeLocks(),
		credsRoot: t.TempDir(),
		cacheGetterFactory: func() (efcCacheGetter, error) {
			factoryCalls++
			return nil, errors.New("must not be called")
		},
	}
	req := publishReq("cpfs-1", t.TempDir()+"/target", map[string]string{}, nil, nil)
	_, err := ns.NodePublishVolume(t.Context(), req)
	require.NoError(t, err)
	assert.Zero(t, factoryCalls)
}

func TestNodePublishVolumeResolvesCacheWithoutDNSPrecheck(t *testing.T) {
	object := validEFCCache()
	require.NoError(t, unstructured.SetNestedField(object.Object, "does-not-resolve", "status", "serviceName"))
	getter := &fakeEFCCacheGetter{object: object}
	ns := &nodeServer{
		mounter:     k8smount.NewFakeMounter(nil),
		locks:       utils.NewVolumeLocks(),
		credsRoot:   t.TempDir(),
		cacheGetter: getter,
	}
	req := publishReq(testFilesystemID, t.TempDir()+"/target", map[string]string{
		efcCacheRefAttribute: "default/training/data",
		_path:                "/subdirectory",
	}, nil, []string{"user_option=true"})
	_, err := ns.NodePublishVolume(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, 1, getter.calls)

	mountPoints := ns.mounter.(*k8smount.FakeMounter).MountPoints
	require.Len(t, mountPoints, 1)
	assert.Equal(t, "10.0.0.1:/subdirectory", mountPoints[0].Device)
	options := mountPoints[0].Opts
	assert.Contains(t, options, "g_tier_DadiDomainName=does-not-resolve.default.svc.cluster.local")
	assert.Contains(t, options, "g_tier_DadiDomainPort=17980")
	assert.Contains(t, options, "g_server_CacheId=training-uid")
	assert.Contains(t, options, "g_distributed_RootClientType=2")
	assert.Contains(t, options, "g_distributed_ReliableRootlistEnable=true")
	assert.Contains(t, options, "g_distributed_NodeListFromCacheEnable=true")
	assert.Contains(t, options, "g_tier_DadiUseHashRingRootlist=true")
	assert.False(t, containsOptionKey(options, "g_tier_DadiAddr"))
	assert.True(t, containsOptionKey(options, "bindtag"))
}

func TestNodePublishVolumeCompatibleSubpathsShareBindTag(t *testing.T) {
	getter := &fakeEFCCacheGetter{object: validEFCCache()}
	ns := &nodeServer{
		mounter:     k8smount.NewFakeMounter(nil),
		locks:       utils.NewVolumeLocks(),
		credsRoot:   t.TempDir(),
		cacheGetter: getter,
	}
	for _, path := range []string{"/dataset/a", "/dataset/b"} {
		req := publishReq(testFilesystemID, t.TempDir()+"/target", map[string]string{
			efcCacheRefAttribute: "default/training/data",
			_path:                path,
		}, nil, []string{"b=2", "a=1"})
		_, err := ns.NodePublishVolume(t.Context(), req)
		require.NoError(t, err)
	}
	mountPoints := ns.mounter.(*k8smount.FakeMounter).MountPoints
	require.Len(t, mountPoints, 2)
	first := optionWithPrefix(mountPoints[0].Opts, "bindtag=")
	second := optionWithPrefix(mountPoints[1].Opts, "bindtag=")
	assert.NotEmpty(t, first)
	assert.Equal(t, first, second)
}

func optionWithPrefix(options []string, prefix string) string {
	for _, option := range normalizedMountFlags(options) {
		if strings.HasPrefix(option, prefix) {
			return option
		}
	}
	return ""
}

func TestNodePublishVolumeCacheRepublishDoesNotCallGetter(t *testing.T) {
	getter := &fakeEFCCacheGetter{object: validEFCCache()}
	ns := &nodeServer{
		mounter:     k8smount.NewFakeMounter(nil),
		locks:       utils.NewVolumeLocks(),
		credsRoot:   t.TempDir(),
		cacheGetter: getter,
	}
	req := publishReq(testFilesystemID, t.TempDir()+"/target", map[string]string{
		efcCacheRefAttribute: "default/training/data",
	}, nil, nil)
	_, err := ns.NodePublishVolume(t.Context(), req)
	require.NoError(t, err)
	_, err = ns.NodePublishVolume(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, 1, getter.calls)
}

var _ efcCacheGetter = (*fakeEFCCacheGetter)(nil)
