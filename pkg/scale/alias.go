// Copyright © 2022 Banzai Cloud
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

package scale

import (
	"github.com/banzaicloud/go-cruise-control/pkg/types"
)

const (
	KafkaBrokerAlive    = types.BrokerStateAlive
	KafkaBrokerDead     = types.BrokerStateDead
	KafkaBrokerNew      = types.BrokerStateNew
	KafkaBrokerDemoted  = types.BrokerStateDemoted
	KafkaBrokerBadDisks = types.BrokerStateBadDisks
)

type KafkaBrokerState = types.BrokerState
