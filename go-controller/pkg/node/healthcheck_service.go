package node

import (
	"fmt"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/healthcheck"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	apierrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
)

// initLoadBalancerHealthChecker initializes the health check server for
// ServiceTypeLoadBalancer services

type loadBalancerHealthChecker struct {
	sync.Mutex
	nodeName     string
	server       healthcheck.Server
	services     map[ktypes.NamespacedName]uint16
	endpoints    map[ktypes.NamespacedName]int
	watchFactory factory.NodeWatchFactory
}

func newLoadBalancerHealthChecker(nodeName string, watchFactory factory.NodeWatchFactory) *loadBalancerHealthChecker {
	return &loadBalancerHealthChecker{
		nodeName:     nodeName,
		server:       healthcheck.NewServer(nodeName, nil, nil, nil),
		services:     make(map[ktypes.NamespacedName]uint16),
		endpoints:    make(map[ktypes.NamespacedName]int),
		watchFactory: watchFactory,
	}
}

func (l *loadBalancerHealthChecker) AddService(svc *kapi.Service) error {
	if svc.Spec.HealthCheckNodePort != 0 {
		l.Lock()
		defer l.Unlock()
		name := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
		l.services[name] = uint16(svc.Spec.HealthCheckNodePort)
		return l.server.SyncServices(l.services)
	}
	return nil
}

func (l *loadBalancerHealthChecker) UpdateService(old, new *kapi.Service) error {
	// if the ETP values have changed between local and cluster,
	// we need to update health checks accordingly
	// HealthCheckNodePort is used only in ETP=local mode
	var err error
	var errors []error
	if old.Spec.ExternalTrafficPolicy == kapi.ServiceExternalTrafficPolicyTypeCluster &&
		new.Spec.ExternalTrafficPolicy == kapi.ServiceExternalTrafficPolicyTypeLocal {
		if err = l.AddService(new); err != nil {
			errors = append(errors, err)
		}
		epSlices, err := l.watchFactory.GetEndpointSlices(new.Namespace, new.Name)
		if err != nil {
			errors = append(errors, fmt.Errorf("could not fetch endpointslices "+
				"for service %s/%s during health check update service: %v",
				new.Namespace, new.Name, err))
			return apierrors.NewAggregate(errors)
		}
		namespacedName := ktypes.NamespacedName{Namespace: new.Namespace, Name: new.Name}
		l.Lock()
		l.endpoints[namespacedName] = l.CountLocalReadyEndpointAddresses(epSlices)
		if err = l.server.SyncEndpoints(l.endpoints); err != nil { // careful
			errors = append(errors, err)
		}
		l.Unlock()
	}
	if old.Spec.ExternalTrafficPolicy == kapi.ServiceExternalTrafficPolicyTypeLocal &&
		new.Spec.ExternalTrafficPolicy == kapi.ServiceExternalTrafficPolicyTypeCluster {
		if err = l.DeleteService(old); err != nil {
			errors = append(errors, err)
		}
	}
	return apierrors.NewAggregate(errors)
}

func (l *loadBalancerHealthChecker) DeleteService(svc *kapi.Service) error {
	if svc.Spec.HealthCheckNodePort != 0 {
		l.Lock()
		defer l.Unlock()
		name := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
		delete(l.services, name)
		delete(l.endpoints, name)
		return l.server.SyncServices(l.services)
	}
	return nil
}

func (l *loadBalancerHealthChecker) SyncServices(svcs []interface{}) error {
	return nil
}

func (l *loadBalancerHealthChecker) SyncEndPointSlices(epSlice *discovery.EndpointSlice) error {
	namespacedName, err := serviceNamespacedNameFromEndpointSlice(epSlice)
	if err != nil {
		return fmt.Errorf("skipping %s/%s: %v", epSlice.Namespace, epSlice.Name, err)
	}
	epSlices, err := l.watchFactory.GetEndpointSlices(epSlice.Namespace, epSlice.Labels[discovery.LabelServiceName])
	if err != nil {
		// should be a rare occurence
		return fmt.Errorf("could not fetch all endpointslices for service %s during health check", namespacedName.String())
	}

	localEndpointAddressCount := l.CountLocalReadyEndpointAddresses(epSlices)
	if len(epSlices) == 0 {
		// let's delete it from cache and wait for the next update;
		// this will show as 0 endpoints for health checks
		delete(l.endpoints, namespacedName)
	} else {
		l.endpoints[namespacedName] = localEndpointAddressCount
	}
	return l.server.SyncEndpoints(l.endpoints)
}

func (l *loadBalancerHealthChecker) AddEndpointSlice(epSlice *discovery.EndpointSlice) error {
	namespacedName, err := serviceNamespacedNameFromEndpointSlice(epSlice)
	if err != nil {
		return fmt.Errorf("cannot add %s/%s to loadBalancerHealthChecker: %v", epSlice.Namespace, epSlice.Name, err)
	}
	l.Lock()
	defer l.Unlock()
	if _, exists := l.services[namespacedName]; exists {
		return l.SyncEndPointSlices(epSlice)
	}
	return nil
}

func (l *loadBalancerHealthChecker) UpdateEndpointSlice(oldEpSlice, newEpSlice *discovery.EndpointSlice) error {
	namespacedName, err := serviceNamespacedNameFromEndpointSlice(newEpSlice)
	if err != nil {
		return fmt.Errorf("cannot update %s/%s in loadBalancerHealthChecker: %v",
			newEpSlice.Namespace, newEpSlice.Name, err)
	}
	l.Lock()
	defer l.Unlock()
	if _, exists := l.services[namespacedName]; exists {
		return l.SyncEndPointSlices(newEpSlice)
	}
	return nil
}

func (l *loadBalancerHealthChecker) DeleteEndpointSlice(epSlice *discovery.EndpointSlice) error {
	l.Lock()
	defer l.Unlock()
	return l.SyncEndPointSlices(epSlice)
}

// CountReadyLocalEndpointAddresses returns the number of IP addresses from ready
// endpoints that are local to the node for a service. This is used to determine the
// response to LB health checks for services with externalTrafficPolicy=local. We consider
// the ready field for health checks and the serving field for setting up services, so that we can
// steer traffic to a local terminating endpoint (ready=false, serving=true, terminating=true),
// while responding negatively to its service health checks, giving time to LBs to update their
// cache of available nodes.
func (l *loadBalancerHealthChecker) CountLocalReadyEndpointAddresses(endpointSlices []*discovery.EndpointSlice) int {
	localEndpointAddresses := sets.NewString()
	for _, endpointSlice := range endpointSlices {
		for _, endpoint := range endpointSlice.Endpoints {
			isLocal := endpoint.NodeName != nil && *endpoint.NodeName == l.nodeName
			if util.IsEndpointReady(endpoint) && isLocal {
				localEndpointAddresses.Insert(endpoint.Addresses...)
			}
		}
	}
	return len(localEndpointAddresses)
}
