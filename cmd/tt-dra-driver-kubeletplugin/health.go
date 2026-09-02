// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: The Kubernetes Authors
// SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc.

/*
 * Copyright The Kubernetes Authors
 * Modifications Copyright 2026 Tenstorrent USA, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

// healthcheck exposes a gRPC health-check endpoint that probes the kubelet
// registration and DRA sockets owned by this driver.
type healthcheck struct {
	grpc_health_v1.UnimplementedHealthServer

	server *grpc.Server
	wg     sync.WaitGroup

	regClient registerapi.RegistrationClient
	draClient drapb.DRAPluginClient
}

// startHealthcheck starts the gRPC healthcheck service when configured. It
// returns nil if the service is disabled (negative port).
func startHealthcheck(ctx context.Context, config *Config) (*healthcheck, error) {
	log := klog.FromContext(ctx)

	port := config.flags.healthcheckPort
	if port < 0 {
		return nil, nil
	}

	addr := net.JoinHostPort("", strconv.Itoa(port))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen for healthcheck service at %s: %w", addr, err)
	}

	regSockPath := (&url.URL{
		Scheme: "unix",
		Path: func() string {
			if config.flags.podUID != "" {
				return path.Join(config.flags.kubeletRegistrarDirectoryPath, config.flags.driverName+"-"+config.flags.podUID+"-reg.sock")
			}
			return path.Join(config.flags.kubeletRegistrarDirectoryPath, config.flags.driverName+"-reg.sock")
		}(),
	}).String()
	log.Info("connecting to registration socket", "path", regSockPath)
	regConn, err := grpc.NewClient(
		regSockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to registration socket: %w", err)
	}

	draSockPath := (&url.URL{
		Scheme: "unix",
		Path: func() string {
			if config.flags.podUID != "" {
				return path.Join(config.DriverPluginPath(), "dra-"+config.flags.podUID+".sock")
			}
			return path.Join(config.DriverPluginPath(), "dra.sock")
		}(),
	}).String()
	log.Info("connecting to DRA socket", "path", draSockPath)
	draConn, err := grpc.NewClient(
		draSockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to DRA socket: %w", err)
	}

	server := grpc.NewServer()
	hc := &healthcheck{
		server:    server,
		regClient: registerapi.NewRegistrationClient(regConn),
		draClient: drapb.NewDRAPluginClient(draConn),
	}
	grpc_health_v1.RegisterHealthServer(server, hc)

	hc.wg.Add(1)
	go func() {
		defer hc.wg.Done()
		log.Info("starting healthcheck service", "addr", lis.Addr().String())
		if err := server.Serve(lis); err != nil {
			log.Error(err, "failed to serve healthcheck service", "addr", addr)
		}
	}()

	return hc, nil
}

// Stop gracefully shuts down the healthcheck server.
func (h *healthcheck) Stop(logger klog.Logger) {
	if h.server != nil {
		logger.Info("stopping healthcheck service")
		h.server.GracefulStop()
	}
	h.wg.Wait()
}

// Check implements grpc_health_v1.HealthServer.
func (h *healthcheck) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	log := klog.FromContext(ctx)

	knownServices := map[string]struct{}{"": {}, "liveness": {}}
	if _, known := knownServices[req.GetService()]; !known {
		return nil, status.Error(codes.NotFound, "unknown service")
	}

	resp := &grpc_health_v1.HealthCheckResponse{
		Status: grpc_health_v1.HealthCheckResponse_NOT_SERVING,
	}

	info, err := h.regClient.GetInfo(ctx, &registerapi.InfoRequest{})
	if err != nil {
		log.Error(err, "failed to call GetInfo")
		return resp, nil
	}
	log.V(5).Info("Successfully invoked GetInfo", "info", info)

	if _, err := h.draClient.NodePrepareResources(ctx, &drapb.NodePrepareResourcesRequest{}); err != nil {
		log.Error(err, "failed to call NodePrepareResources")
		return resp, nil
	}
	log.V(5).Info("Successfully invoked NodePrepareResources")

	resp.Status = grpc_health_v1.HealthCheckResponse_SERVING
	return resp, nil
}
