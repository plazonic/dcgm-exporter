/*
 * Copyright (c) 2021, NVIDIA CORPORATION.  All rights reserved.
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

package dcgmexporter

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	//	"github.com/NVIDIA/go-dcgm/pkg/dcgm"
	"github.com/sirupsen/logrus"
)

var (
	stateDir = "/run/gpustat"
)

func NewPUSlurmMapper(c *Config) (*PUSlurmMapper, error) {
	logrus.Infof("PU/Slurm metrics collection enabled!")

	return &PUSlurmMapper{
		Config: c,
	}, nil
}

func (p *PUSlurmMapper) Name() string {
	return "PUSlurmMapper"
}

func FindMIGUUID(sysInfo SystemInfo, gpu string, instanceId string) string {
	gpuidtemp, err := strconv.ParseUint(gpu, 10, 32)
	if err != nil {
		logrus.Fatalf("Got metric with GPU id %s that is not a positive integer", gpu)
	}
	gpuid := uint(gpuidtemp)
	if gpuid >= sysInfo.GPUCount {
		logrus.Fatalf("Got metric with gpu id %s which is bigger than the number of GPUs %d", gpu, sysInfo.GPUCount)
	}
	migidtemp, err2 := strconv.ParseUint(instanceId, 10, 32)
	if err2 != nil {
		logrus.Fatalf("Got metric for GPU #%s and MIG instance id %s that is not a positive integer", gpu, instanceId)
	}
	migid := uint(migidtemp)
	for j := uint(0); int(j) < len(sysInfo.GPUs[gpuid].GPUInstances); j++ {
		if sysInfo.GPUs[gpuid].GPUInstances[j].Info.NvmlInstanceId == migid {
			return sysInfo.GPUs[gpuid].GPUInstances[j].UUID
		}
	}

	logrus.Warnf("Got metric for GPU #%s and MIG instance id %s that I cannot find in sysinfo.", gpu, instanceId)
	return ""
}

func (p *PUSlurmMapper) Process(metrics [][]Metric, sysInfo SystemInfo) error {
	// e.g. jobiDs["0.4"] = 31212, for GPU#0, MIG nvml instanceid 4, owned by jobid "31212"
	jobIds := make(map[string]string)
	// e.g. userIds[1] = 221290, for GPU#1, no MIG, owned by userid "221290"
	userIds := make(map[string]string)

	// calculate AlterValue
	for i, device := range metrics {
		for j, val := range device {
			var gpuID string
			var jobId string
			var userId string
			var ok bool
			//logrus.Infof("Got field=%s, multiplier=%d, value=%s", val.Counter.FieldName, val.Counter.Multiplier, val.Value)
			if val.Counter.Multiplier != 1 {
				if strings.Contains(val.Value, ".") {
					newval, _ := strconv.ParseFloat(val.Value, 64)
					// for loop range uses copies of values, which is why we modify original metrics
					metrics[i][j].AlterValue = fmt.Sprintf("%f", newval*float64(val.Counter.Multiplier))
				} else {
					newval, _ := strconv.Atoi(val.Value)
					// for loop range uses copies of values, which is why we modify original metrics
					metrics[i][j].AlterValue = fmt.Sprintf("%d", newval*val.Counter.Multiplier)
				}
			} else {
				metrics[i][j].AlterValue = val.Value
			}
			// either just gpuid (say 2) or if MIG gpuid.gpuinstanceid (say 2.11)
			if val.MigProfile != "" {
				gpuID = val.GPU + "." + val.GPUInstanceID
			} else {
				gpuID = val.GPU
			}
			jobId, ok = jobIds[gpuID]
			if ok {
				userId, _ = userIds[gpuID]
			} else {
				// First time checking for this combo
				var gpuIDtemp string
				if val.MigProfile != "" {
					gpuIDtemp = FindMIGUUID(sysInfo, val.GPU, val.GPUInstanceID)
				} else {
					gpuIDtemp = val.GPUUUID
				}
				jobId, userId = CollectJobInfo(stateDir, gpuIDtemp)
				jobIds[gpuID] = jobId
				userIds[gpuID] = userId
			}

			if (jobId != "") && (userId != "") {
				metrics[i][j].Attributes["jobid"] = jobId
				metrics[i][j].Attributes["userid"] = userId
			}
		}
	}
	// Note: for loop are copies the value, if we want to change the value
	// and not the copy, we need to use the indexes
	/*
		for i, device := range metrics {
			for j, val := range device {
				var copyMetric string
				var multiplier int
				switch val.Counter.FieldID {
				case dcgm.DCGM_FI_DEV_GPU_UTIL:
					copyMetric = "nvidia_gpu_duty_cycle"
					multiplier = 1
				case dcgm.DCGM_FI_DEV_FB_FREE:
					copyMetric = "nvidia_gpu_memory_total_bytes"
					multiplier = 1024*1024
				default:
					copyMetric = ""
					multiplier = 1
				}
				if copyMetric != "" {
					newval, _ := strconv.Atoi(val.Value)
					newMetric := Metric{
						Counter:      Counter{val.Counter.FieldID, copyMetric, val.Counter.PromType, val.Counter.Help},
						Value:	      fmt.Sprintf("%d", newval*multiplier),
						UUID:         val.UUID,
			                        GPU:          val.GPU,
					        GPUUUID:      val.GPUUUID,
			                        GPUDevice:    val.GPUDevice,
					        GPUModelName: val.GPUModelName,
			                        Hostname:     val.Hostname,
			                        Labels:	      val.Labels,
					        Attributes:   val.Attributes,
					}
					metrics[i] = append(metrics[i], newMetric)
				}
				logrus.Infof("Got i=%d and devices=%d, metric=%s, copyto=%s, multiplier=%d",i,j, val.Counter.FieldName, copyMetric, multiplier)
			}
		}
	*/

	return nil
}

func CollectJobInfo(dir string, gpu string) (string, string) {
	slurmInfo := fmt.Sprintf("/run/gpustat/%s", gpu)
	if _, err := os.Stat(slurmInfo); err == nil {
		content, err := os.ReadFile(slurmInfo)
		if err == nil {
			var jobUid, jobId string = "", ""
			n, err := fmt.Sscanf(string(content), "%s %s", &jobId, &jobUid)
			if (err == nil) && (n == 2) {
				return jobId, jobUid
			} else {
				logrus.Infof("Ignoring jobinfo file %s, we read %d values, error is %v and contents = %s", slurmInfo, n, err, content)
			}
		} else {
			logrus.Infof("Failed to read f=%s, err=%s", slurmInfo, err)
		}
	}

	return "", ""
}
