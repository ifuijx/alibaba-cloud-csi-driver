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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const (
	efcCacheRefAttribute = "efcCacheRef"
	cacheBindTagPrefix   = "bmcpfs-efc-cache-v1-"
)

var efcCacheGVR = schema.GroupVersionResource{
	Group: "efc.aliyun.com", Version: "v1alpha1", Resource: "efccaches",
}

// The first two labels of a CPFS mount target are the filesystem ID, e.g.
// cpfs-<id>-vpc-<suffix>.<region>.cpfs.aliyuncs.com
var cpfsMountTargetPattern = regexp.MustCompile(`^([0-9a-z]+-[0-9a-z]+)(?:-[-0-9a-z]+)?\.[-0-9a-z]+\.cpfs(?:\.\w+)?\.com$`)

func cpfsFilesystemID(mountTarget string) (string, bool) {
	match := cpfsMountTargetPattern.FindStringSubmatch(strings.ToLower(mountTarget))
	if match == nil {
		return "", false
	}
	return match[1], true
}

type efcCacheReference struct {
	namespace string
	cache     string
	source    string
}

type efcCacheMountConfig struct {
	domain       string
	rootPort     int64
	cacheID      string
	netType      string
	sourceScheme string
}

type efcCacheGetter interface {
	Get(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error)
}

type dynamicEFCCacheGetter struct {
	client dynamic.Interface
}

func (g *dynamicEFCCacheGetter) Get(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	return g.client.Resource(efcCacheGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

func newDynamicEFCCacheGetter() (efcCacheGetter, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("create in-cluster Kubernetes config: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes dynamic client: %w", err)
	}
	return &dynamicEFCCacheGetter{client: client}, nil
}

func parseEFCCacheReference(value string) (efcCacheReference, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 3 {
		return efcCacheReference{}, fmt.Errorf("%s must have exactly namespace/cache/source", efcCacheRefAttribute)
	}
	for index, part := range parts {
		if errs := validation.IsDNS1123Label(part); len(errs) != 0 {
			return efcCacheReference{}, fmt.Errorf("invalid %s component %q: %s", []string{"namespace", "cache", "source"}[index], part, strings.Join(errs, "; "))
		}
	}
	return efcCacheReference{namespace: parts[0], cache: parts[1], source: parts[2]}, nil
}

func (ns *nodeServer) cacheGetterForPublish() (efcCacheGetter, error) {
	ns.cacheGetterMu.Lock()
	defer ns.cacheGetterMu.Unlock()
	if ns.cacheGetter != nil {
		return ns.cacheGetter, nil
	}
	factory := ns.cacheGetterFactory
	if factory == nil {
		factory = newDynamicEFCCacheGetter
	}
	getter, err := factory()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "initialize EFCCache API client: %v", err)
	}
	ns.cacheGetter = getter
	return getter, nil
}

func (ns *nodeServer) resolveEFCCache(ctx context.Context, value, filesystemID string) (efcCacheMountConfig, error) {
	ref, err := parseEFCCacheReference(value)
	if err != nil {
		return efcCacheMountConfig{}, status.Error(codes.InvalidArgument, err.Error())
	}
	getter, err := ns.cacheGetterForPublish()
	if err != nil {
		return efcCacheMountConfig{}, err
	}
	object, err := getter.Get(ctx, ref.namespace, ref.cache)
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err) {
			return efcCacheMountConfig{}, status.Errorf(codes.FailedPrecondition,
				"EFCCache %s/%s is unavailable; ensure the EFCCache CRD and object exist: %v", ref.namespace, ref.cache, err)
		}
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return efcCacheMountConfig{}, status.Errorf(codes.PermissionDenied,
				"get EFCCache %s/%s: grant the CSI node ServiceAccount get access to efccaches.efc.aliyun.com: %v", ref.namespace, ref.cache, err)
		}
		return efcCacheMountConfig{}, status.Errorf(codes.Unavailable,
			"get EFCCache %s/%s from the Kubernetes API: %v", ref.namespace, ref.cache, err)
	}
	return validateEFCCache(object, ref, filesystemID)
}

// validateEFCCache only requires the cache source to be the same CPFS
// filesystem as the volume; mount target, network and subpath may differ.
func validateEFCCache(object *unstructured.Unstructured, ref efcCacheReference, filesystemID string) (efcCacheMountConfig, error) {
	invalid := func(format string, args ...any) (efcCacheMountConfig, error) {
		return efcCacheMountConfig{}, status.Errorf(codes.FailedPrecondition,
			"EFCCache %s/%s is not usable: %s", ref.namespace, ref.cache, fmt.Sprintf(format, args...))
	}
	if object == nil {
		return invalid("API returned an empty object")
	}
	if object.GetDeletionTimestamp() != nil {
		return invalid("object is being deleted")
	}
	observedGeneration, found, err := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	if err != nil || !found || observedGeneration != object.GetGeneration() {
		return invalid("status.observedGeneration must equal metadata.generation")
	}
	phase, found, err := unstructured.NestedString(object.Object, "status", "phase")
	if err != nil || !found || phase == "" {
		return invalid("status.phase is required")
	}
	if phase == "Failed" {
		return invalid("status.phase is Failed")
	}
	cacheID, found, err := unstructured.NestedString(object.Object, "status", "cacheID")
	if err != nil || !found || cacheID == "" {
		return invalid("status.cacheID is required")
	}
	if errs := validation.IsDNS1123Subdomain(cacheID); len(errs) != 0 {
		return invalid("status.cacheID %q is invalid: %s", cacheID, strings.Join(errs, "; "))
	}
	serviceName, found, err := unstructured.NestedString(object.Object, "status", "serviceName")
	if err != nil || !found || serviceName == "" {
		return invalid("status.serviceName is required")
	}
	if errs := validation.IsDNS1123Label(serviceName); len(errs) != 0 {
		return invalid("status.serviceName %q is invalid: %s", serviceName, strings.Join(errs, "; "))
	}
	rootPort, err := sourceRootPort(object.Object, ref.source)
	if err != nil {
		return invalid("%v", err)
	}
	sourceURI, err := sourceURIForName(object.Object, ref.source)
	if err != nil {
		return invalid("%v", err)
	}
	parsed, err := url.Parse(sourceURI)
	if err != nil {
		return invalid("parse spec.sources[%q].source: %v", ref.source, err)
	}
	if parsed.Scheme != "cpfs-vpc" && parsed.Scheme != "cpfs-pov" {
		return invalid("spec.sources[%q].source scheme must be cpfs-vpc or cpfs-pov", ref.source)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return invalid("spec.sources[%q].source must be a CPFS URI with only host and path", ref.source)
	}
	sourceFilesystemID, ok := cpfsFilesystemID(parsed.Host)
	if !ok {
		return invalid("spec.sources[%q].source host %q is not a CPFS mount target with a filesystem ID", ref.source, parsed.Host)
	}
	if sourceFilesystemID != filesystemID {
		return invalid("spec.sources[%q].source filesystem %q does not match volume filesystem %q", ref.source, sourceFilesystemID, filesystemID)
	}
	netType, found, err := unstructured.NestedString(object.Object, "spec", "cache", "options", "netType")
	if err != nil {
		return invalid("spec.cache.options.netType must be a string")
	}
	if !found || netType == "" {
		netType = "tcp"
	}
	switch netType {
	case "tcp", "erdma", "mlx", "eic":
	default:
		return invalid("unsupported spec.cache.options.netType %q", netType)
	}
	return efcCacheMountConfig{
		domain:       fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, ref.namespace),
		rootPort:     rootPort,
		cacheID:      cacheID,
		netType:      netType,
		sourceScheme: parsed.Scheme,
	}, nil
}

func sourceRootPort(object map[string]any, sourceName string) (int64, error) {
	sources, found, err := unstructured.NestedSlice(object, "status", "sources")
	if err != nil || !found {
		return 0, fmt.Errorf("status.sources is required")
	}
	for _, item := range sources {
		source, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(source, "name")
		if name != sourceName {
			continue
		}
		port, found, err := unstructured.NestedInt64(source, "rootPort")
		if err != nil || !found || port < 1 || port > 65535 {
			return 0, fmt.Errorf("status.sources[%q].rootPort must be between 1 and 65535", sourceName)
		}
		return port, nil
	}
	return 0, fmt.Errorf("status.sources has no source named %q", sourceName)
}

func sourceURIForName(object map[string]any, sourceName string) (string, error) {
	sources, found, err := unstructured.NestedSlice(object, "spec", "sources")
	if err != nil || !found {
		return "", fmt.Errorf("spec.sources is required")
	}
	for _, item := range sources {
		source, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(source, "name")
		if name != sourceName {
			continue
		}
		value, found, err := unstructured.NestedString(source, "source")
		if err != nil || !found || value == "" {
			return "", fmt.Errorf("spec.sources[%q].source is required", sourceName)
		}
		return value, nil
	}
	return "", fmt.Errorf("spec.sources has no source named %q", sourceName)
}

var cacheManagedMountOptionKeys = map[string]struct{}{
	"g_tier_EnableClusterCache":             {},
	"g_tier_DadiIsDistributed":              {},
	"g_tier_DadiRootClientType":             {},
	"g_distributed_RootClientType":          {},
	"g_distributed_ReliableRootlistEnable":  {},
	"g_distributed_NodeListFromCacheEnable": {},
	"g_tier_DadiUseHashRingRootlist":        {},
	"g_tier_EnableSharedCache":              {},
	"g_tier_DadiDomainName":                 {},
	"g_tier_DadiDomainPort":                 {},
	"g_tier_DadiNetType":                    {},
	"g_tier_EnableBusyRDMA":                 {},
	"g_tier_DadiDiskAio":                    {},
	"g_cpfs_SizeTrusted":                    {},
	"g_server_CacheId":                      {},
	"bindtag":                               {},
}

func validateCacheManagedMountOptions(options []string) error {
	for _, option := range normalizedMountFlags(options) {
		key, _, _ := strings.Cut(option, "=")
		if _, managed := cacheManagedMountOptionKeys[key]; managed {
			return status.Errorf(codes.InvalidArgument,
				"mount options must not contain driver-managed cache option %q when %s is set", key, efcCacheRefAttribute)
		}
	}
	return nil
}

func cacheMountOptions(config efcCacheMountConfig) []string {
	options := []string{
		"g_tier_EnableClusterCache=true",
		"g_tier_DadiIsDistributed=true",
		"g_tier_DadiRootClientType=2",
		"g_distributed_RootClientType=2",
		"g_distributed_ReliableRootlistEnable=true",
		"g_distributed_NodeListFromCacheEnable=true",
		"g_tier_DadiUseHashRingRootlist=true",
		"g_tier_EnableSharedCache=false",
		"g_tier_DadiDomainName=" + config.domain,
		fmt.Sprintf("g_tier_DadiDomainPort=%d", config.rootPort),
		"g_server_CacheId=" + config.cacheID,
		"g_tier_DadiNetType=" + dadiNetType(config.netType),
	}
	switch config.netType {
	case "mlx":
		options = append(options, "g_tier_EnableBusyRDMA=false")
	case "eic":
		options = append(options, "g_tier_EnableBusyRDMA=true")
	}
	return append(options, "g_tier_DadiDiskAio=true", "g_cpfs_SizeTrusted=true")
}

func dadiNetType(netType string) string {
	switch netType {
	case "erdma":
		return "2"
	case "mlx", "eic":
		return "1"
	default:
		return "0"
	}
}

func normalizedMountFlags(options []string) []string {
	normalized := make([]string, 0, len(options))
	for _, group := range options {
		for _, option := range strings.Split(group, ",") {
			option = strings.TrimSpace(option)
			if option == "-o" {
				continue
			}
			if strings.HasPrefix(option, "-o ") {
				option = strings.TrimSpace(strings.TrimPrefix(option, "-o "))
			}
			if option != "" {
				normalized = append(normalized, option)
			}
		}
	}
	sort.Strings(normalized)
	return normalized
}

func cacheBindTag(mountTarget, backendNetworkType string, config efcCacheMountConfig, userMountFlags, authOptions []string) (string, error) {
	authOptions = append([]string(nil), authOptions...)
	sort.Strings(authOptions)
	canonical := struct {
		MountTarget       string   `json:"mountTarget"`
		BackendNetwork    string   `json:"backendNetwork"`
		CacheDomain       string   `json:"cacheDomain"`
		CachePort         int64    `json:"cachePort"`
		CacheID           string   `json:"cacheID"`
		CacheNetwork      string   `json:"cacheNetwork"`
		SourceScheme      string   `json:"sourceScheme"`
		CacheMountOptions []string `json:"cacheMountOptions"`
		UserMountFlags    []string `json:"userMountFlags"`
		AuthMountOptions  []string `json:"authMountOptions"`
	}{
		MountTarget: mountTarget, BackendNetwork: backendNetworkType,
		CacheDomain: config.domain, CachePort: config.rootPort, CacheID: config.cacheID,
		CacheNetwork: config.netType, SourceScheme: config.sourceScheme,
		CacheMountOptions: cacheMountOptions(config),
		UserMountFlags:    normalizedMountFlags(userMountFlags), AuthMountOptions: authOptions,
	}
	serialized, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("serialize cache root configuration: %w", err)
	}
	digest := sha256.Sum256(serialized)
	return cacheBindTagPrefix + hex.EncodeToString(digest[:])[:20], nil
}
