/*
 * Copyright 2015 DGraph Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * 		http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package loader

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/dgryski/go-farm"

	"github.com/dgraph-io/dgraph/posting"
	"github.com/dgraph-io/dgraph/rdf"
	"github.com/dgraph-io/dgraph/store"
	"github.com/dgraph-io/dgraph/x"
)

var glog = x.Log("loader")
var dataStore *store.Store

var maxRoutines = flag.Int("maxroutines", 3000,
	"Maximum number of goroutines to execute concurrently")

type counters struct {
	read      uint64
	parsed    uint64
	processed uint64
	ignored   uint64
}

type state struct {
	sync.RWMutex
	input        chan string
	cnq          chan rdf.NQuad
	ctr          *counters
	instanceIdx  uint64
	numInstances uint64
	err          error
}

func Init(datastore *store.Store) {
	dataStore = datastore
}

func (s *state) Error() error {
	s.RLock()
	defer s.RUnlock()
	return s.err
}

func (s *state) SetError(err error) {
	s.Lock()
	defer s.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// printCounters prints the counter variables at intervals
// specified by ticker.
func (s *state) printCounters(ticker *time.Ticker) {
	var prev uint64
	for _ = range ticker.C {
		processed := atomic.LoadUint64(&s.ctr.processed)
		if prev == processed {
			continue
		}
		prev = processed
		parsed := atomic.LoadUint64(&s.ctr.parsed)
		ignored := atomic.LoadUint64(&s.ctr.ignored)
		pending := parsed - ignored - processed
		glog.WithFields(logrus.Fields{
			"read":      atomic.LoadUint64(&s.ctr.read),
			"processed": processed,
			"parsed":    parsed,
			"ignored":   ignored,
			"pending":   pending,
			"len_cnq":   len(s.cnq),
		}).Info("Counters")
	}
}

// Reads a single line from a buffered reader. The line is read into the
// passed in buffer to minimize allocations. This is the preferred
// method for loading long lines which could be longer than the buffer
// size of bufio.Scanner.
func readLine(r *bufio.Reader, buf *bytes.Buffer) error {
	isPrefix := true
	var err error
	buf.Reset()
	for isPrefix && err == nil {
		var line []byte
		// The returned line is an internal buffer in bufio and is only
		// valid until the next call to ReadLine. It needs to be copied
		// over to our own buffer.
		line, isPrefix, err = r.ReadLine()
		if err == nil {
			buf.Write(line)
		}
	}
	return err
}

// readLines reads the file and pushes the nquads onto a channel.
// Run this in a single goroutine. This function closes s.input channel.
func (s *state) readLines(r io.Reader) {
	var buf []string
	var err error
	var strBuf bytes.Buffer
	bufReader := bufio.NewReader(r)
	// Randomize lines to avoid contention on same subject.
	for i := 0; i < 1000; i++ {
		err = readLine(bufReader, &strBuf)
		if err != nil {
			break
		}
		buf = append(buf, strBuf.String())
		atomic.AddUint64(&s.ctr.read, 1)
	}

	if err != nil && err != io.EOF {
		err := x.Errorf("Error while reading file: %v", err)
		log.Fatalf("%+v", err)
	}

	// If we haven't yet finished reading the file read the rest of the rows.
	for {
		err = readLine(bufReader, &strBuf)
		if err != nil {
			break
		}
		k := rand.Intn(len(buf))
		s.input <- buf[k]
		buf[k] = strBuf.String()
		atomic.AddUint64(&s.ctr.read, 1)
	}

	if err != io.EOF {
		err := x.Errorf("Error while reading file: %v", err)
		log.Fatalf("%+v", err)
	}
	for i := 0; i < len(buf); i++ {
		s.input <- buf[i]
	}
	close(s.input)
}

// parseStream consumes the lines, converts them to nquad
// and sends them into cnq channel.
func (s *state) parseStream(wg *sync.WaitGroup) {
	defer wg.Done()

	for line := range s.input {
		if s.Error() != nil {
			return
		}
		line = strings.Trim(line, " \t")
		if len(line) == 0 {
			glog.Info("Empty line.")
			continue
		}

		glog.Debugf("Got line: %q", line)
		nq, err := rdf.Parse(line)
		if err != nil {
			s.SetError(err)
			return
		}
		s.cnq <- nq
		atomic.AddUint64(&s.ctr.parsed, 1)
	}
}

// handleNQuads converts the nQuads that satisfy the modulo
// rule into posting lists.
func (s *state) handleNQuads(wg *sync.WaitGroup) {
	defer wg.Done()

	ctx := context.Background()
	for nq := range s.cnq {
		if s.Error() != nil {
			return
		}
		// Only handle this edge if the attribute satisfies the modulo rule
		if farm.Fingerprint64([]byte(nq.Predicate))%s.numInstances != s.instanceIdx {
			atomic.AddUint64(&s.ctr.ignored, 1)
			continue
		}

		edge, err := nq.ToEdge()
		for err != nil {
			// Just put in a retry loop to tackle temporary errors.
			if err == posting.E_TMP_ERROR {
				time.Sleep(time.Microsecond)

			} else {
				s.SetError(err)
				glog.WithError(err).WithField("nq", nq).
					Error("While converting to edge")
				return
			}
			edge, err = nq.ToEdge()
		}

		key := posting.Key(edge.Entity, edge.Attribute)

		plist, decr := posting.GetOrCreate(key, dataStore)
		plist.AddMutationWithIndex(ctx, edge, posting.Set)
		decr() // Don't defer, just call because we're in a channel loop.

		atomic.AddUint64(&s.ctr.processed, 1)
	}
}

// LoadEdges is called with the reader object of a file whose
// contents are to be converted to posting lists.
func LoadEdges(reader io.Reader, instanceIdx uint64,
	numInstances uint64) (uint64, error) {

	s := new(state)
	s.ctr = new(counters)
	ticker := time.NewTicker(time.Second)
	go s.printCounters(ticker)

	// Producer: Start buffering input to channel.
	s.instanceIdx = instanceIdx
	s.numInstances = numInstances
	s.input = make(chan string, 10000)
	go s.readLines(reader)

	s.cnq = make(chan rdf.NQuad, 10000)
	numr := runtime.GOMAXPROCS(-1)
	var pwg sync.WaitGroup
	pwg.Add(numr)
	for i := 0; i < numr; i++ {
		go s.parseStream(&pwg) // Input --> NQuads
	}

	nrt := *maxRoutines
	var wg sync.WaitGroup
	wg.Add(nrt)
	for i := 0; i < nrt; i++ {
		go s.handleNQuads(&wg) // NQuads --> Posting list [slow].
	}

	// Block until all parseStream goroutines are finished.
	pwg.Wait()
	close(s.cnq)
	// Okay, we've stopped input to cnq, and closed it.
	// Now wait for handleNQuads to finish.
	wg.Wait()

	ticker.Stop()
	return atomic.LoadUint64(&s.ctr.processed), s.Error()
}
