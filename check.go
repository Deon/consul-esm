package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	consulchecks "github.com/hashicorp/consul/agent/checks"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/consul/types"
)

const externalCheckName = "externalNodeHealth"

type CheckRunner struct {
	sync.RWMutex

	logger *log.Logger
	client *api.Client

	checks         map[types.CheckID]*api.HealthCheck
	checksHTTP     map[types.CheckID]*consulchecks.CheckHTTP
	checksTCP      map[types.CheckID]*consulchecks.CheckTCP
	checksCritical map[types.CheckID]time.Time

	// Used to track checks that are being deferred
	deferCheck map[types.CheckID]*time.Timer

	CheckUpdateInterval time.Duration
}

func NewCheckRunner(logger *log.Logger, client *api.Client, updateInterval time.Duration) *CheckRunner {
	return &CheckRunner{
		logger:              logger,
		client:              client,
		checks:              make(map[types.CheckID]*api.HealthCheck),
		checksHTTP:          make(map[types.CheckID]*consulchecks.CheckHTTP),
		checksTCP:           make(map[types.CheckID]*consulchecks.CheckTCP),
		checksCritical:      make(map[types.CheckID]time.Time),
		deferCheck:          make(map[types.CheckID]*time.Timer),
		CheckUpdateInterval: updateInterval,
	}
}

func (c *CheckRunner) Stop() {
	c.Lock()
	defer c.Unlock()

	for _, check := range c.checksHTTP {
		check.Stop()
	}

	for _, check := range c.checksTCP {
		check.Stop()
	}
}

// UpdateChecks takes a list of checks from the catalog and updates
// our list of running checks to match.
func (c *CheckRunner) UpdateChecks(checks api.HealthChecks) {
	c.Lock()
	defer c.Unlock()

	found := make(map[types.CheckID]struct{})

	for _, check := range checks {
		// Skip the ping-based node check since we're managing that separately
		if check.CheckID == externalCheckName {
			continue
		}

		checkHash := checkHash(check)
		found[checkHash] = struct{}{}
		if _, ok := c.checks[checkHash]; ok {
			continue
		}

		definition := check.Definition
		if definition.HTTP != "" {
			http := &consulchecks.CheckHTTP{
				Notify:        c,
				CheckID:       checkHash,
				HTTP:          definition.HTTP,
				Header:        definition.Header,
				Method:        definition.Method,
				TLSSkipVerify: definition.TLSSkipVerify,
				Interval:      definition.Interval.Duration(),
				Timeout:       definition.Timeout.Duration(),
				Logger:        c.logger,
			}

			http.Start()
			c.checksHTTP[checkHash] = http
		} else if definition.TCP != "" {
			tcp := &consulchecks.CheckTCP{
				Notify:   c,
				CheckID:  checkHash,
				TCP:      definition.TCP,
				Interval: definition.Interval.Duration(),
				Timeout:  definition.Timeout.Duration(),
				Logger:   c.logger,
			}

			tcp.Start()
			c.checksTCP[checkHash] = tcp
		} else {
			c.logger.Printf("[WARN] check %q is not a valid HTTP or TCP check", checkHash)
			continue
		}

		c.checks[checkHash] = check
	}

	// Look for removed checks
	for _, check := range c.checks {
		checkHash := checkHash(check)
		if _, ok := found[checkHash]; !ok {
			delete(c.checks, checkHash)
			delete(c.checksCritical, checkHash)
			if check.Definition.HTTP != "" {
				httpCheck := c.checksHTTP[checkHash]
				httpCheck.Stop()
				delete(c.checksHTTP, checkHash)
			} else {
				tcpCheck := c.checksTCP[checkHash]
				tcpCheck.Stop()
				delete(c.checksTCP, checkHash)
			}
		}
	}
}

// UpdateCheck handles the output of an HTTP/TCP check and decides whether or not
// to push an update to the catalog.
func (c *CheckRunner) UpdateCheck(checkID types.CheckID, status, output string) {
	c.Lock()
	defer c.Unlock()

	check, ok := c.checks[checkID]
	if !ok {
		return
	}

	// Update the critical time tracking
	if status == api.HealthCritical {
		if _, ok := c.checksCritical[checkID]; !ok {
			c.checksCritical[checkID] = time.Now()
		}
	} else {
		delete(c.checksCritical, checkID)
	}

	// Do nothing if update is idempotent
	if check.Status == status && check.Output == output {
		return
	}

	// Defer a sync if the output has changed. This is an optimization around
	// frequent updates of output. Instead, we update the output internally,
	// and periodically do a write-back to the servers. If there is a status
	// change we do the write immediately.
	if c.CheckUpdateInterval > 0 && check.Status == status {
		check.Output = output
		if _, ok := c.deferCheck[checkID]; !ok {
			intv := time.Duration(uint64(c.CheckUpdateInterval)/2) + lib.RandomStagger(c.CheckUpdateInterval)
			deferSync := time.AfterFunc(intv, func() {
				c.Lock()
				c.handleCheckUpdate(check, status, output)
				delete(c.deferCheck, checkID)
				c.Unlock()
			})
			c.deferCheck[checkID] = deferSync
		}
		return
	}

	c.handleCheckUpdate(check, status, output)
}

// handleCheckUpdate writes a check's status to the catalog and updates the local check state.
// Should only be called when the lock is held.
func (c *CheckRunner) handleCheckUpdate(check *api.HealthCheck, status, output string) {
	reg := &api.CatalogRegistration{
		Node: check.Node,
		Check: &api.AgentCheck{
			CheckID:     strings.TrimPrefix(string(check.CheckID), check.Node+"/"),
			Name:        check.Name,
			Status:      status,
			Notes:       check.Notes,
			Output:      output,
			ServiceID:   check.ServiceID,
			ServiceName: check.ServiceName,
			Definition:  check.Definition,
		},
		SkipNodeUpdate: true,
	}
	_, err := c.client.Catalog().Register(reg, nil)
	if err != nil {
		c.logger.Printf("[WARN] Error updating check status in Consul: %v", err)
		return
	}

	// Only update the local check state if we successfully updated the catalog
	check.Status = status
	check.Output = output
}

// reapServices is a long running goroutine that looks for checks that have been
// critical too long and deregisters their associated services.
func (c *CheckRunner) reapServices(shutdownCh <-chan struct{}) {
	for {
		select {
		case <-time.After(30 * time.Second):
			c.reapServicesInternal()

		case <-shutdownCh:
			return
		}
	}
}

// reapServicesInternal does a single pass, looking for services to reap.
func (c *CheckRunner) reapServicesInternal() {
	c.Lock()
	defer c.Unlock()

	reaped := make(map[string]bool)
	for checkID, criticalTime := range c.checksCritical {
		check := c.checks[checkID]
		serviceID := check.ServiceID

		// There's nothing to do if there's no service.
		if serviceID == "" {
			continue
		}

		// There might be multiple checks for one service, so
		// we don't need to reap multiple times.
		if reaped[serviceID] {
			continue
		}

		timeout := check.Definition.DeregisterCriticalServiceAfter.Duration()
		if timeout > 0 && timeout > time.Since(criticalTime) {
			c.client.Catalog().Deregister(&api.CatalogDeregistration{
				Node:      check.Node,
				ServiceID: serviceID,
			}, nil)
			c.logger.Printf("[INFO] agent: Check %q for service %q has been critical for too long; deregistered service",
				checkID, serviceID)
			reaped[serviceID] = true
		}
	}
}

func checkHash(check *api.HealthCheck) types.CheckID {
	return types.CheckID(fmt.Sprintf("%s/%s", check.Node, check.CheckID))
}
