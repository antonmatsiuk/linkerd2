package public

import (
	"context"
	"reflect"

	proto "github.com/golang/protobuf/proto"
	"github.com/linkerd/linkerd2/controller/api/util"
	pb "github.com/linkerd/linkerd2/controller/gen/public"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type resourceResult struct {
	res *pb.StatTable
	err error
}

type k8sStat struct {
	object   metav1.Object
	podStats *podStats
}

type rKey struct {
	Namespace string
	Type      string
	Name      string
}

const (
	success = "success"
	failure = "failure"

	reqQuery             = "sum(increase(response_total%s[%s])) by (%s, classification, tls)"
	latencyQuantileQuery = "histogram_quantile(%s, sum(irate(response_latency_ms_bucket%s[%s])) by (le, %s))"
	tcpConnectionsQuery  = "sum(tcp_open_connections%s) by (%s)"
	tcpReadBytesQuery    = "sum(increase(tcp_read_bytes_total%s[%s])) by (%s)"
	tcpWriteBytesQuery   = "sum(increase(tcp_write_bytes_total%s[%s])) by (%s)"
)

type podStats struct {
	status string
	inMesh uint64
	total  uint64
	failed uint64
	errors map[string]*pb.PodErrors
}

func (s *grpcServer) StatSummary(ctx context.Context, req *pb.StatSummaryRequest) (*pb.StatSummaryResponse, error) {

	// check for well-formed request
	if req.GetSelector().GetResource() == nil {
		return statSummaryError(req, "StatSummary request missing Selector Resource"), nil
	}

	// special case to check for services as outbound only
	if isInvalidServiceRequest(req.Selector, req.GetFromResource()) {
		return statSummaryError(req, "service only supported as a target on 'from' queries, or as a destination on 'to' queries"), nil
	}

	switch req.Outbound.(type) {
	case *pb.StatSummaryRequest_ToResource:
		if req.Outbound.(*pb.StatSummaryRequest_ToResource).ToResource.Type == k8s.All {
			return statSummaryError(req, "resource type 'all' is not supported as a filter"), nil
		}
	case *pb.StatSummaryRequest_FromResource:
		if req.Outbound.(*pb.StatSummaryRequest_FromResource).FromResource.Type == k8s.All {
			return statSummaryError(req, "resource type 'all' is not supported as a filter"), nil
		}
	}

	statTables := make([]*pb.StatTable, 0)

	var resourcesToQuery []string
	if req.Selector.Resource.Type == k8s.All {
		resourcesToQuery = k8s.StatAllResourceTypes
	} else {
		resourcesToQuery = []string{req.Selector.Resource.Type}
	}

	// request stats for the resourcesToQuery, in parallel
	resultChan := make(chan resourceResult)

	for _, resource := range resourcesToQuery {
		statReq := proto.Clone(req).(*pb.StatSummaryRequest)
		statReq.Selector.Resource.Type = resource

		go func() {
			if isNonK8sResourceQuery(statReq.GetSelector().GetResource().GetType()) {
				resultChan <- s.nonK8sResourceQuery(ctx, statReq)
			} else {
				resultChan <- s.k8sResourceQuery(ctx, statReq)
			}
		}()
	}

	for i := 0; i < len(resourcesToQuery); i++ {
		result := <-resultChan
		if result.err != nil {
			return nil, util.GRPCError(result.err)
		}
		statTables = append(statTables, result.res)
	}

	rsp := pb.StatSummaryResponse{
		Response: &pb.StatSummaryResponse_Ok_{ // https://github.com/golang/protobuf/issues/205
			Ok: &pb.StatSummaryResponse_Ok{
				StatTables: statTables,
			},
		},
	}

	return &rsp, nil
}

func isInvalidServiceRequest(selector *pb.ResourceSelection, fromResource *pb.Resource) bool {
	if fromResource != nil {
		return fromResource.Type == k8s.Service
	}

	return selector.Resource.Type == k8s.Service
}

func statSummaryError(req *pb.StatSummaryRequest, message string) *pb.StatSummaryResponse {
	return &pb.StatSummaryResponse{
		Response: &pb.StatSummaryResponse_Error{
			Error: &pb.ResourceError{
				Resource: req.GetSelector().GetResource(),
				Error:    message,
			},
		},
	}
}

func (s *grpcServer) getKubernetesObjectStats(req *pb.StatSummaryRequest) (map[rKey]k8sStat, error) {
	requestedResource := req.GetSelector().GetResource()
	objects, err := s.k8sAPI.GetObjects(requestedResource.Namespace, requestedResource.Type, requestedResource.Name)
	if err != nil {
		return nil, err
	}

	objectMap := map[rKey]k8sStat{}

	for _, object := range objects {
		metaObj, err := meta.Accessor(object)
		if err != nil {
			return nil, err
		}

		key := rKey{
			Name:      metaObj.GetName(),
			Namespace: metaObj.GetNamespace(),
			Type:      requestedResource.GetType(),
		}

		podStats, err := s.getPodStats(object)
		if err != nil {
			return nil, err
		}

		objectMap[key] = k8sStat{
			object:   metaObj,
			podStats: podStats,
		}
	}
	return objectMap, nil
}

func (s *grpcServer) k8sResourceQuery(ctx context.Context, req *pb.StatSummaryRequest) resourceResult {
	k8sObjects, err := s.getKubernetesObjectStats(req)
	if err != nil {
		return resourceResult{res: nil, err: err}
	}

	var requestMetrics map[rKey]*pb.BasicStats
	var tcpMetrics map[rKey]*pb.TcpStats
	if !req.SkipStats {
		requestMetrics, tcpMetrics, err = s.getStatMetrics(ctx, req, req.TimeWindow)
		if err != nil {
			return resourceResult{res: nil, err: err}
		}
	}

	rows := make([]*pb.StatTable_PodGroup_Row, 0)
	keys := getResultKeys(req, k8sObjects, requestMetrics)

	for _, key := range keys {
		objInfo, ok := k8sObjects[key]
		if !ok {
			continue
		}

		var tcpStats *pb.TcpStats
		if req.TcpStats {
			tcpStats = tcpMetrics[key]
		}

		var basicStats *pb.BasicStats
		if !reflect.DeepEqual(requestMetrics[key], &pb.BasicStats{}) {
			basicStats = requestMetrics[key]
		}

		k8sResource := objInfo.object
		row := pb.StatTable_PodGroup_Row{
			Resource: &pb.Resource{
				Name:      k8sResource.GetName(),
				Namespace: k8sResource.GetNamespace(),
				Type:      req.GetSelector().GetResource().GetType(),
			},
			TimeWindow: req.TimeWindow,
			Stats:      basicStats,
			TcpStats:   tcpStats,
		}

		podStat := objInfo.podStats
		row.Status = podStat.status
		row.MeshedPodCount = podStat.inMesh
		row.RunningPodCount = podStat.total
		row.FailedPodCount = podStat.failed
		row.ErrorsByPod = podStat.errors

		rows = append(rows, &row)
	}

	rsp := pb.StatTable{
		Table: &pb.StatTable_PodGroup_{
			PodGroup: &pb.StatTable_PodGroup{
				Rows: rows,
			},
		},
	}

	return resourceResult{res: &rsp, err: nil}
}

func (s *grpcServer) nonK8sResourceQuery(ctx context.Context, req *pb.StatSummaryRequest) resourceResult {
	var requestMetrics map[rKey]*pb.BasicStats
	if !req.SkipStats {
		var err error
		requestMetrics, _, err = s.getStatMetrics(ctx, req, req.TimeWindow)
		if err != nil {
			return resourceResult{res: nil, err: err}
		}
	}
	rows := make([]*pb.StatTable_PodGroup_Row, 0)

	for rkey, metrics := range requestMetrics {
		rkey.Type = req.GetSelector().GetResource().GetType()

		row := pb.StatTable_PodGroup_Row{
			Resource: &pb.Resource{
				Type:      rkey.Type,
				Namespace: rkey.Namespace,
				Name:      rkey.Name,
			},
			TimeWindow: req.TimeWindow,
			Stats:      metrics,
		}
		rows = append(rows, &row)
	}

	rsp := pb.StatTable{
		Table: &pb.StatTable_PodGroup_{
			PodGroup: &pb.StatTable_PodGroup{
				Rows: rows,
			},
		},
	}
	return resourceResult{res: &rsp, err: nil}
}

func isNonK8sResourceQuery(resourceType string) bool {
	return resourceType == k8s.Authority
}

// get the list of objects for which we want to return results
func getResultKeys(
	req *pb.StatSummaryRequest,
	k8sObjects map[rKey]k8sStat,
	metricResults map[rKey]*pb.BasicStats,
) []rKey {
	var keys []rKey

	if req.GetOutbound() == nil || req.GetNone() != nil {
		// if the request doesn't have outbound filtering, return all rows
		for key := range k8sObjects {
			keys = append(keys, key)
		}
	} else {
		// if the request does have outbound filtering,
		// only return rows for which we have stats
		for key := range metricResults {
			keys = append(keys, key)
		}
	}
	return keys
}

func buildRequestLabels(req *pb.StatSummaryRequest) (labels model.LabelSet, labelNames model.LabelNames) {
	// labelNames: the group by in the prometheus query
	// labels: the labels for the resource we want to query for

	switch out := req.Outbound.(type) {
	case *pb.StatSummaryRequest_ToResource:
		labelNames = promGroupByLabelNames(req.Selector.Resource)

		labels = labels.Merge(promDstQueryLabels(out.ToResource))
		labels = labels.Merge(promQueryLabels(req.Selector.Resource))
		labels = labels.Merge(promDirectionLabels("outbound"))

	case *pb.StatSummaryRequest_FromResource:
		labelNames = promDstGroupByLabelNames(req.Selector.Resource)

		labels = labels.Merge(promQueryLabels(out.FromResource))
		labels = labels.Merge(promDstQueryLabels(req.Selector.Resource))
		labels = labels.Merge(promDirectionLabels("outbound"))

	default:
		labelNames = promGroupByLabelNames(req.Selector.Resource)

		labels = labels.Merge(promQueryLabels(req.Selector.Resource))
		labels = labels.Merge(promDirectionLabels("inbound"))
	}

	return
}

func (s *grpcServer) getStatMetrics(ctx context.Context, req *pb.StatSummaryRequest, timeWindow string) (map[rKey]*pb.BasicStats, map[rKey]*pb.TcpStats, error) {
	reqLabels, groupBy := buildRequestLabels(req)
	promQueries := map[promType]string{
		promRequests: reqQuery,
	}

	if req.TcpStats {
		promQueries[promTCPConnections] = tcpConnectionsQuery
		promQueries[promTCPReadBytes] = tcpReadBytesQuery
		promQueries[promTCPWriteBytes] = tcpWriteBytesQuery
	}
	results, err := s.getPrometheusMetrics(ctx, promQueries, latencyQuantileQuery, reqLabels.String(), timeWindow, groupBy.String())

	if err != nil {
		return nil, nil, err
	}

	basicStats, tcpStats := processPrometheusMetrics(req, results, groupBy)
	return basicStats, tcpStats, nil
}

func processPrometheusMetrics(req *pb.StatSummaryRequest, results []promResult, groupBy model.LabelNames) (map[rKey]*pb.BasicStats, map[rKey]*pb.TcpStats) {
	basicStats := make(map[rKey]*pb.BasicStats)
	tcpStats := make(map[rKey]*pb.TcpStats)

	for _, result := range results {
		for _, sample := range result.vec {
			resource := metricToKey(req, sample.Metric, groupBy)

			addBasicStats := func() {
				if basicStats[resource] == nil {
					basicStats[resource] = &pb.BasicStats{}
				}
			}
			addTCPStats := func() {
				if tcpStats[resource] == nil {
					tcpStats[resource] = &pb.TcpStats{}
				}
			}

			value := extractSampleValue(sample)

			switch result.prom {
			case promRequests:
				addBasicStats()
				switch string(sample.Metric[model.LabelName("classification")]) {
				case success:
					basicStats[resource].SuccessCount += value
				case failure:
					basicStats[resource].FailureCount += value
				}
			case promLatencyP50:
				addBasicStats()
				basicStats[resource].LatencyMsP50 = value
			case promLatencyP95:
				addBasicStats()
				basicStats[resource].LatencyMsP95 = value
			case promLatencyP99:
				addBasicStats()
				basicStats[resource].LatencyMsP99 = value
			case promTCPConnections:
				addTCPStats()
				tcpStats[resource].OpenConnections = value
			case promTCPReadBytes:
				addTCPStats()
				tcpStats[resource].ReadBytesTotal = value
			case promTCPWriteBytes:
				addTCPStats()
				tcpStats[resource].WriteBytesTotal = value
			}

		}
	}

	return basicStats, tcpStats
}

func metricToKey(req *pb.StatSummaryRequest, metric model.Metric, groupBy model.LabelNames) rKey {
	// this key is used to match the metric stats we queried from prometheus
	// with the k8s object stats we queried from k8s
	// ASSUMPTION: this code assumes that groupBy is always ordered (..., namespace, name)
	key := rKey{
		Type: req.GetSelector().GetResource().GetType(),
		Name: string(metric[groupBy[len(groupBy)-1]]),
	}

	if len(groupBy) == 2 {
		key.Namespace = string(metric[groupBy[0]])
	}

	return key
}

func (s *grpcServer) getPodStats(obj runtime.Object) (*podStats, error) {
	pods, err := s.k8sAPI.GetPodsFor(obj, true)
	if err != nil {
		return nil, err
	}
	podErrors := make(map[string]*pb.PodErrors)
	meshCount := &podStats{}

	if pod, ok := obj.(*corev1.Pod); ok {
		meshCount.status = k8s.GetPodStatus(*pod)
	}

	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodFailed {
			meshCount.failed++
		} else {
			meshCount.total++
			if k8s.IsMeshed(pod, s.controllerNamespace) {
				meshCount.inMesh++
			}
		}

		errors := checkContainerErrors(pod.Status.ContainerStatuses)
		errors = append(errors, checkContainerErrors(pod.Status.InitContainerStatuses)...)

		if len(errors) > 0 {
			podErrors[pod.Name] = &pb.PodErrors{Errors: errors}
		}
	}
	meshCount.errors = podErrors
	return meshCount, nil
}

func toPodError(container, image, reason, message string) *pb.PodErrors_PodError {
	return &pb.PodErrors_PodError{
		Error: &pb.PodErrors_PodError_Container{
			Container: &pb.PodErrors_PodError_ContainerError{
				Message:   message,
				Container: container,
				Image:     image,
				Reason:    reason,
			},
		},
	}
}

func checkContainerErrors(containerStatuses []corev1.ContainerStatus) []*pb.PodErrors_PodError {
	errors := []*pb.PodErrors_PodError{}
	for _, st := range containerStatuses {
		if !st.Ready {
			if st.State.Waiting != nil {
				errors = append(errors, toPodError(st.Name, st.Image, st.State.Waiting.Reason, st.State.Waiting.Message))
			}

			if st.State.Terminated != nil && (st.State.Terminated.ExitCode != 0 || st.State.Terminated.Signal != 0) {
				errors = append(errors, toPodError(st.Name, st.Image, st.State.Terminated.Reason, st.State.Terminated.Message))
			}

			if st.LastTerminationState.Waiting != nil {
				errors = append(errors, toPodError(st.Name, st.Image, st.LastTerminationState.Waiting.Reason, st.LastTerminationState.Waiting.Message))
			}

			if st.LastTerminationState.Terminated != nil {
				errors = append(errors, toPodError(st.Name, st.Image, st.LastTerminationState.Terminated.Reason, st.LastTerminationState.Terminated.Message))
			}
		}
	}
	return errors
}
