/*
Copyright 2018 Sysdig.

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

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sort"
)

func getMetrics(hostname string) (metricValue float64, err error) {
	hostFilter := fmt.Sprintf(`host.hostName = '%s'`, hostname)
	start := -60
	end := 0
	sampling := 60

	metricDataResponse, err := sysdigAPI.GetData(metrics, start, end, sampling, hostFilter, "host")
	if err != nil {
		return
	} else if metricDataResponse.StatusCode != 200 {
		err = fmt.Errorf("metric data response: %s", metricDataResponse.Status)
		return
	}
	defer metricDataResponse.Body.Close()

	all, err := ioutil.ReadAll(metricDataResponse.Body)

	var metricData struct {
		Data []struct {
			D []float64 `json:"d"`
		} `json:"data"`
	}

	err = json.Unmarshal(all, &metricData)
	if err != nil {
		return
	}

	if len(metricData.Data) > 0 && len(metricData.Data[0].D) > 0 {
		metricValue = metricData.Data[0].D[0]
	} else {
		err = noDataFound
	}

	return
}

var bestNodeMutex sync.Mutex
func getBestNodeByMetrics(nodes []string) (bestNodeFound Node, err error) {
	bestNodeMutex.Lock()
	defer bestNodeMutex.Unlock()

	if len(nodes) == 0 {
		err = emptyNodeList
		return
	}

	if cachedNodes, ok := cachedNodes.Data(); ok {
		if reflect.DeepEqual(cachedNodes, nodes) {
			if bestNode, ok := bestCachedNode.Data(); ok {
				log.Println("Using cache...")
				return bestNode.(Node), nil
			}
		}
	}

	// We will make all the request asynchronous for performance reasons
	wg := sync.WaitGroup{}
	nodeStatsChannel := make(chan Node, len(nodes))
	nodeStatsErrorsChannel := make(chan Node, len(nodes))

	// Launch all requests asynchronously
	for _, node := range nodes {
		wg.Add(1)

		go func(nodeName string) {
			defer wg.Done()

			split := strings.Split(nodeName, ".")
			nodeNameLittle := split[0]

			metricsValue, err := getMetrics(nodeNameLittle)
			if err == nil { // No error found, we will send the struct
				nodeStatsChannel <- Node{name: nodeName, metric: metricsValue}
			} else {
				nodeStatsErrorsChannel <- Node{name: nodeName, err: err}
			}
		}(node)
	}

	wg.Wait()
	close(nodeStatsChannel)
	close(nodeStatsErrorsChannel)

	nodeList := NodeList{}
	for node := range nodeStatsChannel {
		nodeList = append(nodeList, node)
	}
	bestNodeFound = bestNodeFromList(nodeList)

	errorHappenedString := `Error retrieving node "%s": "%s"`
	for node := range nodeStatsErrorsChannel {
		log.Printf(errorHappenedString+"\n", node.name, node.err.Error())
	}

	if bestNodeFound.name == "" || bestNodeFound.metric == -1 {
		err = noNodeFound
	}

	if err == nil {
		bestCachedNode.SetData(bestNodeFound)
	}

	return
}

func bestNodeFromList(list NodeList) (node Node) {
	sort.Sort(list)
	lenght := len(list)
	if lenght > 0 {
		if sysdigMetricLower {
			return list[0] // Get the first -> Lower
		} else {
			return list[lenght-1] // Get the last -> Higher
		}
	}
	return
}

func nodesAvailable() (readyNodes []string) {
	if nodes, ok := cachedNodes.Data(); ok {
		return nodes.([]string)
	}

	nodes, err := kubeAPI.ListNodes()
	if err != nil {
		log.Println(err)
	}
	for _, node := range nodes {
		for _, status := range node.Status.Conditions {
			if status.Status == "True" && status.Type == "Ready" {
				readyNodes = append(readyNodes, node.Metadata.Name)
			}
		}
	}

	cachedNodes.SetData(readyNodes)
	return
}

func scheduler(podName, nodeName, namespace string) (response *http.Response, err error) {
	if namespace == "" {
		namespace = "default"
	}

	body := map[string]interface{}{
		"target": map[string]string{
			"kind":       "Node",
			"apiVersion": "v1",
			"name":       nodeName,
			"namespace":  namespace,
		},
		"metadata": map[string]string{
			"name":      podName,
			"namespace": namespace,
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return
	}

	return kubeAPI.CreateNamespacedBinding(namespace, bytes.NewReader(data))
}
