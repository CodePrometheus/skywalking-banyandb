// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package measure implements a time-series-based storage which is consists of a sequence of data points.
// Each data point contains tags and fields. They arrive in a fixed interval. A data point could be updated
// by one with the identical entity(series_id) and timestamp.
package measure

import (
	"time"

	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	databasev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/database/v1"
	"github.com/apache/skywalking-banyandb/banyand/queue"
	"github.com/apache/skywalking-banyandb/pkg/logger"
	"github.com/apache/skywalking-banyandb/pkg/partition"
	"github.com/apache/skywalking-banyandb/pkg/query/logical"
	"github.com/apache/skywalking-banyandb/pkg/run"
	"github.com/apache/skywalking-banyandb/pkg/schema"
	"github.com/apache/skywalking-banyandb/pkg/timestamp"
)

const (
	maxValuesBlockSize              = 8 * 1024 * 1024
	maxTagFamiliesMetadataSize      = 8 * 1024 * 1024
	maxUncompressedBlockSize        = 2 * 1024 * 1024
	maxUncompressedPrimaryBlockSize = 128 * 1024

	maxBlockLength = 8 * 1024

	defaultFlushTimeout = 5 * time.Second
)

type option struct {
	mergePolicy        *mergePolicy
	flushTimeout       time.Duration
	seriesCacheMaxSize run.Bytes
}

type measure struct {
	databaseSupplier   schema.Supplier
	indexTagMap        map[string]struct{}
	l                  *logger.Logger
	schema             *databasev1.Measure
	processorManager   *topNProcessorManager
	fieldIndexLocation partition.FieldIndexLocation
	name               string
	group              string
	indexRuleLocators  partition.IndexRuleLocator
	indexRules         []*databasev1.IndexRule
	topNAggregations   []*databasev1.TopNAggregation
	interval           time.Duration
	shardNum           uint32
}

func (s *measure) startSteamingManager(pipeline queue.Queue) error {
	if len(s.topNAggregations) == 0 {
		return nil
	}
	tagMapSpec := logical.TagSpecMap{}
	tagMapSpec.RegisterTagFamilies(s.schema.GetTagFamilies())

	s.processorManager = &topNProcessorManager{
		l:            s.l,
		pipeline:     pipeline,
		m:            s,
		s:            tagMapSpec,
		topNSchemas:  s.topNAggregations,
		processorMap: make(map[*commonv1.Metadata][]*topNStreamingProcessor),
	}

	return s.processorManager.start()
}

func (s *measure) GetSchema() *databasev1.Measure {
	return s.schema
}

func (s *measure) GetIndexRules() []*databasev1.IndexRule {
	return s.indexRules
}

func (s *measure) Close() error {
	if s.processorManager == nil {
		return nil
	}
	return s.processorManager.Close()
}

func (s *measure) parseSpec() (err error) {
	s.name, s.group = s.schema.GetMetadata().GetName(), s.schema.GetMetadata().GetGroup()
	if s.schema.Interval != "" {
		s.interval, err = timestamp.ParseDuration(s.schema.Interval)
	}
	s.indexRuleLocators, s.fieldIndexLocation = partition.ParseIndexRuleLocators(s.schema.GetEntity(), s.schema.GetTagFamilies(), s.indexRules, s.schema.IndexMode)
	s.indexTagMap = make(map[string]struct{})
	for j := range s.indexRules {
		for k := range s.indexRules[j].Tags {
			s.indexTagMap[s.indexRules[j].Tags[k]] = struct{}{}
		}
	}
	return err
}

type measureSpec struct {
	schema           *databasev1.Measure
	indexRules       []*databasev1.IndexRule
	topNAggregations []*databasev1.TopNAggregation
}

func openMeasure(shardNum uint32, db schema.Supplier, spec measureSpec, l *logger.Logger, pipeline queue.Queue,
) (*measure, error) {
	m := &measure{
		shardNum:         shardNum,
		schema:           spec.schema,
		indexRules:       spec.indexRules,
		topNAggregations: spec.topNAggregations,
		l:                l,
	}
	if err := m.parseSpec(); err != nil {
		return nil, err
	}
	if db == nil {
		return m, nil
	}

	m.databaseSupplier = db
	if startErr := m.startSteamingManager(pipeline); startErr != nil {
		l.Err(startErr).Str("measure", spec.schema.GetMetadata().GetName()).
			Msg("fail to start streaming manager")
	}
	return m, nil
}
