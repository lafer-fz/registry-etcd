// Copyright 2021 CloudWeGo Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package etcd resolver
package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/kitex/pkg/klog"
	"github.com/cloudwego/kitex/pkg/registry"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	ttlKey              = "KITEX_ETCD_REGISTRY_LEASE_TTL"
	defaultTTL          = 60
	kitexIpToRegistry   = "KITEX_IP_TO_REGISTRY"
	kitexPortToRegistry = "KITEX_PORT_TO_REGISTRY"
	maxRetryKey         = "KITEX_ETCD_REGISTRY_MAX_RETRY"
	defaultMaxRetry     = 5
)

type etcdRegistry struct {
	etcdClient *clientv3.Client
	leaseTTL   int64
	maxRetry   int
	meta       *registerMeta
}

type registerMeta struct {
	leaseID clientv3.LeaseID
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewEtcdRegistry creates an etcd based registry.
func NewEtcdRegistry(endpoints []string, opts ...Option) (registry.Registry, error) {
	cfg := clientv3.Config{
		Endpoints: endpoints,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	etcdClient, err := clientv3.New(cfg)
	if err != nil {
		return nil, err
	}
	return &etcdRegistry{
		etcdClient: etcdClient,
		leaseTTL:   getTTL(),
		maxRetry:   getMaxRetry(),
	}, nil
}

// NewEtcdRegistryWithAuth creates a etcd based registry with given username and password.
// Deprecated: Use WithAuthOpt instead.
func NewEtcdRegistryWithAuth(endpoints []string, username, password string) (registry.Registry, error) {
	etcdClient, err := clientv3.New(clientv3.Config{
		Endpoints: endpoints,
		Username:  username,
		Password:  password,
	})
	if err != nil {
		return nil, err
	}
	return &etcdRegistry{
		etcdClient: etcdClient,
		leaseTTL:   getTTL(),
		maxRetry:   getMaxRetry(),
	}, nil
}

// Register registers a server with given registry info.
func (e *etcdRegistry) Register(info *registry.Info) error {
	if err := validateRegistryInfo(info); err != nil {
		return err
	}
	leaseID, err := e.register(info)
	if err != nil {
		return err
	}
	meta := registerMeta{
		leaseID: leaseID,
	}
	meta.ctx, meta.cancel = context.WithCancel(context.Background())
	e.meta = &meta
	go e.keepalive(info, &meta)
	return nil
}

// Deregister deregisters a server with given registry info.
func (e *etcdRegistry) Deregister(info *registry.Info) error {
	if info.ServiceName == "" {
		return fmt.Errorf("missing service name in Deregister")
	}
	if err := e.deregister(info); err != nil {
		return err
	}
	e.meta.cancel()
	return nil
}

func (e *etcdRegistry) register(info *registry.Info) (clientv3.LeaseID, error) {
	addr, err := e.getAddressOfRegistration(info)
	if err != nil {
		return clientv3.NoLease, err
	}
	leaseID, err := e.grantLease()
	if err != nil {
		return clientv3.NoLease, err
	}
	val, err := json.Marshal(&instanceInfo{
		Network: info.Addr.Network(),
		Address: addr,
		Weight:  info.Weight,
		Tags:    info.Tags,
	})
	if err != nil {
		return clientv3.NoLease, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	_, err = e.etcdClient.Put(ctx, serviceKey(info.ServiceName, addr), string(val), clientv3.WithLease(leaseID))
	if err != nil {
		return clientv3.NoLease, err
	}
	return leaseID, nil
}

func (e *etcdRegistry) deregister(info *registry.Info) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	addr, err := e.getAddressOfRegistration(info)
	if err != nil {
		return err
	}
	_, err = e.etcdClient.Delete(ctx, serviceKey(info.ServiceName, addr))
	return err
}

func (e *etcdRegistry) grantLease() (clientv3.LeaseID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	resp, err := e.etcdClient.Grant(ctx, e.leaseTTL)
	if err != nil {
		return clientv3.NoLease, err
	}
	return resp.ID, nil
}

func (e *etcdRegistry) keepalive(info *registry.Info, meta *registerMeta) {
	curLeaseID := meta.leaseID
	lka, err := e.etcdClient.KeepAlive(meta.ctx, meta.leaseID)
	if err != nil {
		klog.Warnf("failed to keepalive lease %x for etcd registry: %v", curLeaseID, err)
		curLeaseID = clientv3.NoLease
	} else {
		klog.Infof("start keepalive lease %x for etcd registry", curLeaseID)
	}
	rand.Seed(time.Now().Unix())
	for {
		// re-register
		if curLeaseID == clientv3.NoLease {
			var retreat []int
			for retryCnt := 0; retryCnt < e.maxRetry; retryCnt++ {
				retreat = append(retreat, 1<<retryCnt)
				// check if context is done
				if meta.ctx.Err() != nil {
					klog.Infof("context is done for etcd registry")
					return
				}
				id, registerErr := e.register(info)
				// if register failed, retry
				if registerErr != nil {
					klog.Infof("register failed for etcd registry: %v", registerErr)
					time.Sleep(time.Duration(retreat[rand.Intn(len(retreat))]) * time.Second)
					continue
				}
				curLeaseID = id
				lka, err = e.etcdClient.KeepAlive(meta.ctx, curLeaseID)
				// if keepalive succeed, break
				if err == nil {
					break
				}
				klog.Warnf("failed to keepalive lease %x for etcd registry: %v", curLeaseID, err)
				// if re-register failed, wait for a while and retry
				time.Sleep(time.Duration(retreat[rand.Intn(len(retreat))]) * time.Second)
			}
			if _, ok := <-lka; !ok {
				// re-registration failed
				klog.Warnf("re-registration failed for etcd registry")
				return
			}
		}

		select {
		case _, ok := <-lka:
			if !ok {
				// check if context is done
				if meta.ctx.Err() != nil {
					klog.Infof("context is done for etcd registry")
					return
				}
				// need to re-register
				curLeaseID = clientv3.NoLease
				continue
			}
		case <-meta.ctx.Done():
			klog.Infof("stop keepalive lease %x for etcd registry", curLeaseID)
			return
		}
	}
}

// getAddressOfRegistration returns the address of the service registration.
func (e *etcdRegistry) getAddressOfRegistration(info *registry.Info) (string, error) {

	host, port, err := net.SplitHostPort(info.Addr.String())
	if err != nil {
		return "", err
	}

	// if host is empty or "::", use local ipv4 address as host
	if host == "" || host == "::" {
		host, err = getLocalIPv4Host()
		if err != nil {
			return "", fmt.Errorf("parse registry info addr error: %w", err)
		}
	}

	// if env KITEX_IP_TO_REGISTRY is set, use it as host
	if ipToRegistry, exists := os.LookupEnv(kitexIpToRegistry); exists && ipToRegistry != "" {
		host = ipToRegistry
	}

	// if env KITEX_PORT_TO_REGISTRY is set, use it as port
	if portToRegistry, exists := os.LookupEnv(kitexPortToRegistry); exists && portToRegistry != "" {
		port = portToRegistry
	}

	p, err := strconv.Atoi(port)
	if err != nil {
		return "", fmt.Errorf("parse registry info port error: %w", err)
	}

	return fmt.Sprintf("%s:%d", host, p), nil
}

func validateRegistryInfo(info *registry.Info) error {
	if info.ServiceName == "" {
		return fmt.Errorf("missing service name in Register")
	}
	if strings.Contains(info.ServiceName, "/") {
		return fmt.Errorf("service name registered with etcd should not include character '/'")
	}
	if info.Addr == nil {
		return fmt.Errorf("missing addr in Register")
	}
	return nil
}

func getTTL() int64 {
	var ttl int64 = defaultTTL
	if str, ok := os.LookupEnv(ttlKey); ok {
		if t, err := strconv.Atoi(str); err == nil {
			ttl = int64(t)
		}
	}
	return ttl
}

func getMaxRetry() int {
	var maxRetry int = defaultMaxRetry
	if str, ok := os.LookupEnv(maxRetryKey); ok {
		if t, err := strconv.Atoi(str); err == nil {
			maxRetry = t
		}
	}
	return maxRetry
}

func getLocalIPv4Host() (string, error) {
	addr, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addr {
		ipNet, isIpNet := addr.(*net.IPNet)
		if isIpNet && !ipNet.IP.IsLoopback() {
			ipv4 := ipNet.IP.To4()
			if ipv4 != nil {
				return ipv4.String(), nil
			}
		}
	}
	return "", fmt.Errorf("not found ipv4 address")
}
