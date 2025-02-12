// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package enum

import (
	"context"
	"errors"
	"sync"
	"time"

	alts "github.com/OWASP/Amass/v3/alterations"
	"github.com/OWASP/Amass/v3/config"
	eb "github.com/OWASP/Amass/v3/eventbus"
	"github.com/OWASP/Amass/v3/queue"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/services"
	sf "github.com/OWASP/Amass/v3/stringfilter"
	"github.com/OWASP/Amass/v3/stringset"
)

// Filters contains the set of string filters required during an enumeration.
type Filters struct {
	NewNames      *sf.StringFilter
	Resolved      *sf.StringFilter
	NewAddrs      *sf.StringFilter
	SweepAddrs    *sf.StringFilter
	Output        *sf.StringFilter
	PassiveOutput *sf.StringFilter
}

// Enumeration is the object type used to execute a DNS enumeration with Amass.
type Enumeration struct {
	sync.Mutex

	// Information sent in the context
	Config *config.Config
	Bus    *eb.EventBus
	Sys    services.System

	altState    *alts.State
	markovModel *alts.MarkovModel
	startedAlts bool
	altQueue    *queue.Queue

	ctx context.Context

	filters *Filters
	dataMgr services.Service

	startedBrute bool
	bruteQueue   *queue.Queue

	srcsLock sync.Mutex
	srcs     stringset.Set

	// The channel and queue that will receive the results
	Output      chan *requests.Output
	outputQueue *queue.Queue

	// Queue for the log messages
	logQueue *queue.Queue

	// Broadcast channel that indicates no further writes to the output channel
	done              chan struct{}
	doneAlreadyClosed bool

	// Cache for the infrastructure data collected from online sources
	netLock  sync.Mutex
	netCache map[int]*requests.ASNRequest
	netQueue *queue.Queue

	subLock    sync.Mutex
	subdomains map[string]int

	addrsLock sync.Mutex
	addrs     stringset.Set

	lastLock sync.Mutex
	last     time.Time

	perSecLock  sync.Mutex
	perSec      int64
	perSecFirst time.Time
	perSecLast  time.Time
}

// NewEnumeration returns an initialized Enumeration that has not been started yet.
func NewEnumeration(sys services.System) *Enumeration {
	e := &Enumeration{
		Config:   config.NewConfig(),
		Bus:      eb.NewEventBus(),
		Sys:      sys,
		altQueue: new(queue.Queue),
		filters: &Filters{
			NewNames:      sf.NewStringFilter(),
			Resolved:      sf.NewStringFilter(),
			NewAddrs:      sf.NewStringFilter(),
			SweepAddrs:    sf.NewStringFilter(),
			Output:        sf.NewStringFilter(),
			PassiveOutput: sf.NewStringFilter(),
		},
		bruteQueue:  new(queue.Queue),
		srcs:        stringset.New(),
		Output:      make(chan *requests.Output, 100),
		outputQueue: new(queue.Queue),
		logQueue:    new(queue.Queue),
		done:        make(chan struct{}, 2),
		netCache:    make(map[int]*requests.ASNRequest),
		netQueue:    new(queue.Queue),
		subdomains:  make(map[string]int),
		last:        time.Now(),
		perSecFirst: time.Now(),
		perSecLast:  time.Now(),
	}

	if ref := e.refToDataManager(); ref != nil {
		e.dataMgr = ref
		return e
	}
	return nil
}

func (e *Enumeration) refToDataManager() services.Service {
	for _, srv := range e.Sys.CoreServices() {
		if srv.String() == "Data Manager" {
			return srv
		}
	}
	return nil
}

// Done safely closes the done broadcast channel.
func (e *Enumeration) Done() {
	e.Lock()
	defer e.Unlock()

	if !e.doneAlreadyClosed {
		e.doneAlreadyClosed = true
		close(e.done)
	}
}

// Start begins the DNS enumeration process for the Amass Enumeration object.
func (e *Enumeration) Start() error {
	if e.Output == nil {
		return errors.New("The enumeration did not have an output channel")
	} else if e.Config.Passive && e.Config.DataOptsWriter != nil {
		return errors.New("Data operations cannot be saved without DNS resolution")
	} else if err := e.Config.CheckSettings(); err != nil {
		return err
	}

	// Setup the stringset of included data sources
	e.srcsLock.Lock()
	srcs := stringset.New()
	e.srcs.Intersect(srcs)
	srcs.InsertMany(e.Config.SourceFilter.Sources...)
	for _, src := range e.Sys.DataSources() {
		e.srcs.Insert(src.String())
	}
	if srcs.Len() > 0 && e.Config.SourceFilter.Include {
		e.srcs.Intersect(srcs)
	} else {
		e.srcs.Subtract(srcs)
	}
	e.srcsLock.Unlock()

	// Setup the DNS name alteration objects
	e.markovModel = alts.NewMarkovModel(3)
	e.altState = alts.NewState(e.Config.AltWordlist)
	e.altState.MinForWordFlip = e.Config.MinForWordFlip
	e.altState.EditDistance = e.Config.EditDistance

	// Setup the context used throughout the enumeration
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, requests.ContextConfig, e.Config)
	e.ctx = context.WithValue(ctx, requests.ContextEventBus, e.Bus)

	e.setupEventBus()

	e.addrs = stringset.New()
	go e.processAddresses()

	// The enumeration will not terminate until all output has been processed
	var wg sync.WaitGroup
	wg.Add(4)
	// Use all previously discovered names that are in scope
	go e.submitKnownNames(&wg)
	go e.submitProvidedNames(&wg)
	go e.checkForOutput(&wg)
	go e.processOutput(&wg)

	if e.Config.Timeout > 0 {
		time.AfterFunc(time.Duration(e.Config.Timeout)*time.Minute, func() {
			e.Config.Log.Printf("Enumeration exceeded provided timeout")
			e.Done()
		})
	}

	// Release all the domain names specified in the configuration
	e.srcsLock.Lock()
	// Put in requests for all the ASNs specified in the configuration
	for _, src := range e.Sys.DataSources() {
		if !e.srcs.Has(src.String()) {
			continue
		}

		for _, asn := range e.Config.ASNs {
			src.ASNRequest(e.ctx, &requests.ASNRequest{ASN: asn})
		}
	}

	for _, src := range e.Sys.DataSources() {
		if !e.srcs.Has(src.String()) {
			continue
		}

		for _, domain := range e.Config.Domains() {
			// Send each root domain name
			src.DNSRequest(e.ctx, &requests.DNSRequest{
				Name:   domain,
				Domain: domain,
			})
		}
	}
	e.srcsLock.Unlock()

	t := time.NewTicker(2 * time.Second)
	logTick := time.NewTicker(time.Minute)
loop:
	for {
		select {
		case <-e.done:
			break loop
		case <-t.C:
			e.writeLogs()

			// Has the enumeration been inactive long enough to stop the task?
			if inactive := time.Now().Sub(e.lastActive()); inactive > 5*time.Second {
				e.nextPhase(true)
				time.Sleep(time.Second)
			}
		case <-logTick.C:
			if !e.Config.Passive {
				remaining := e.DNSNamesRemaining()

				e.Config.Log.Printf("Average DNS queries performed: %d/sec, DNS names queued: %d",
					e.DNSQueriesPerSec(), remaining)

				// Does the enumeration need more names to process?
				if !e.Config.Passive && remaining < 50 {
					e.nextPhase(false)
				}

				e.clearPerSec()
			}
		}
	}
	t.Stop()
	logTick.Stop()
	cancel()
	e.cleanEventBus()
	time.Sleep(2 * time.Second)
	wg.Wait()
	e.writeLogs()
	return nil
}

func (e *Enumeration) nextPhase(inactive bool) {
	if !e.Config.Passive && (e.DNSQueriesPerSec() > 2000) || (e.DNSNamesRemaining() > 10) {
		return
	}

	if !e.Config.Passive && e.Config.BruteForcing && !e.startedBrute {
		e.startedBrute = true
		go e.startBruteForcing()
		time.Sleep(time.Second)
		e.Config.Log.Print("Starting DNS queries for brute forcing")
	} else if !e.Config.Passive && e.Config.Alterations && !e.startedAlts {
		e.startedAlts = true
		go e.performAlterations()
		time.Sleep(time.Second)
		e.Config.Log.Print("Starting DNS queries for altered names")
	} else if inactive {
		// End the enumeration!
		e.Done()
	}
}

// DNSQueriesPerSec returns the number of DNS queries the enumeration has performed per second.
func (e *Enumeration) DNSQueriesPerSec() int64 {
	e.perSecLock.Lock()
	defer e.perSecLock.Unlock()

	if sec := e.perSecLast.Sub(e.perSecFirst).Seconds(); sec > 0 {
		return e.perSec / int64(sec+1.0)
	}
	return 0
}

func (e *Enumeration) incQueriesPerSec(t time.Time) {
	e.perSecLock.Lock()
	defer e.perSecLock.Unlock()

	e.perSec++
	if t.After(e.perSecLast) {
		e.perSecLast = t
	}
}

func (e *Enumeration) clearPerSec() {
	e.perSecLock.Lock()
	defer e.perSecLock.Unlock()

	e.perSec = 0
	e.perSecFirst = time.Now()
	e.perSecLast = e.perSecLast
}

// DNSNamesRemaining returns the number of discovered DNS names yet to be handled by the enumeration.
func (e *Enumeration) DNSNamesRemaining() int64 {
	var remaining int

	for _, srv := range e.Sys.CoreServices() {
		if srv.String() == "DNS Service" {
			remaining += srv.RequestLen()
			break
		}
	}

	return int64(remaining)
}

func (e *Enumeration) lastActive() time.Time {
	e.lastLock.Lock()
	defer e.lastLock.Unlock()

	return e.last
}

func (e *Enumeration) updateLastActive(srv string) {
	e.lastLock.Lock()
	defer e.lastLock.Unlock()

	e.last = time.Now()
}

func (e *Enumeration) setupEventBus() {
	e.Bus.Subscribe(requests.OutputTopic, e.sendOutput)
	e.Bus.Subscribe(requests.LogTopic, e.queueLog)
	e.Bus.Subscribe(requests.SetActiveTopic, e.updateLastActive)
	e.Bus.Subscribe(requests.ResolveCompleted, e.incQueriesPerSec)

	e.Bus.Subscribe(requests.NewNameTopic, e.newNameEvent)

	if !e.Config.Passive {
		e.Bus.Subscribe(requests.NameResolvedTopic, e.newResolvedName)

		e.Bus.Subscribe(requests.NewAddrTopic, e.newAddress)
		e.Bus.Subscribe(requests.NewASNTopic, e.updateASNCache)
	}

	// Setup all core services to receive the appropriate events
loop:
	for _, srv := range e.Sys.CoreServices() {
		switch srv.String() {
		case "Data Manager":
			// All requests to the data manager will be sent directly
			continue loop
		case "DNS Service":
			e.Bus.Subscribe(requests.ResolveNameTopic, srv.DNSRequest)
			e.Bus.Subscribe(requests.SubDiscoveredTopic, srv.SubdomainDiscovered)
		default:
			e.Bus.Subscribe(requests.NameRequestTopic, srv.DNSRequest)
			e.Bus.Subscribe(requests.SubDiscoveredTopic, srv.SubdomainDiscovered)
			e.Bus.Subscribe(requests.AddrRequestTopic, srv.AddrRequest)
			e.Bus.Subscribe(requests.ASNRequestTopic, srv.ASNRequest)
		}
	}
}

func (e *Enumeration) cleanEventBus() {
	e.Bus.Unsubscribe(requests.OutputTopic, e.sendOutput)
	e.Bus.Unsubscribe(requests.LogTopic, e.queueLog)
	e.Bus.Unsubscribe(requests.SetActiveTopic, e.updateLastActive)
	e.Bus.Unsubscribe(requests.ResolveCompleted, e.incQueriesPerSec)

	e.Bus.Unsubscribe(requests.NewNameTopic, e.newNameEvent)

	if !e.Config.Passive {
		e.Bus.Unsubscribe(requests.NameResolvedTopic, e.newResolvedName)

		e.Bus.Unsubscribe(requests.NewAddrTopic, e.newAddress)
		e.Bus.Unsubscribe(requests.NewASNTopic, e.updateASNCache)
	}

	// Setup all core services to receive the appropriate events
loop:
	for _, srv := range e.Sys.CoreServices() {
		switch srv.String() {
		case "Data Manager":
			// All requests to the data manager will be sent directly
			continue loop
		case "DNS Service":
			e.Bus.Unsubscribe(requests.ResolveNameTopic, srv.DNSRequest)
			e.Bus.Unsubscribe(requests.SubDiscoveredTopic, srv.SubdomainDiscovered)
		default:
			e.Bus.Unsubscribe(requests.NameRequestTopic, srv.DNSRequest)
			e.Bus.Unsubscribe(requests.SubDiscoveredTopic, srv.SubdomainDiscovered)
			e.Bus.Unsubscribe(requests.AddrRequestTopic, srv.AddrRequest)
			e.Bus.Unsubscribe(requests.ASNRequestTopic, srv.ASNRequest)
		}
	}
}
