package handlers

import (
	"errors"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/api/apps/v1beta2"

	"github.com/gorilla/mux"
	"github.com/kiali/kiali/business"
	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/kubernetes"
	"github.com/kiali/kiali/kubernetes/kubetest"
	"github.com/kiali/kiali/prometheus"
	"github.com/kiali/kiali/prometheus/prometheustest"
	osappsv1 "github.com/openshift/api/apps/v1"
	osv1 "github.com/openshift/api/project/v1"
	"github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	batch_v1 "k8s.io/api/batch/v1"
	batch_v1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

func setupWorkloadList() (*httptest.Server, *kubetest.K8SClientMock, *prometheustest.PromClientMock) {
	k8s := kubetest.NewK8SClientMock()
	prom := new(prometheustest.PromClientMock)
	business.SetWithBackends(k8s, prom)

	mr := mux.NewRouter()
	mr.HandleFunc("/api/namespaces/{namespace}/workloads", WorkloadList)

	ts := httptest.NewServer(mr)
	return ts, k8s, prom
}

func TestWorkloadsEndpoint(t *testing.T) {
	conf := config.NewConfig()
	config.Set(conf)
	ts, k8s, _ := setupWorkloadList()
	defer ts.Close()

	k8s.On("GetDeployments", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return(business.FakeDepSyncedWithRS(), nil)
	k8s.On("GetReplicaSets", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return(business.FakeRSSyncedWithPods(), nil)
	k8s.On("GetDeploymentConfigs", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return([]osappsv1.DeploymentConfig{}, nil)
	k8s.On("GetReplicationControllers", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return([]corev1.ReplicationController{}, nil)
	k8s.On("GetStatefulSets", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return([]v1beta2.StatefulSet{}, nil)
	k8s.On("GetJobs", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return([]batch_v1.Job{}, nil)
	k8s.On("GetCronJobs", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return([]batch_v1beta1.CronJob{}, nil)
	k8s.On("GetPods", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return(business.FakePodsSyncedWithDeployments(), nil)

	url := ts.URL + "/api/namespaces/ns/workloads"

	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := ioutil.ReadAll(resp.Body)

	assert.NotEmpty(t, actual)
	assert.Equal(t, 200, resp.StatusCode, string(actual))
	k8s.AssertNumberOfCalls(t, "GetDeployments", 1)
}

func TestWorkloadMetricsDefault(t *testing.T) {
	ts, api, _ := setupWorkloadMetricsEndpoint(t)
	defer ts.Close()

	url := ts.URL + "/api/namespaces/ns/workloads/my_workload/metrics"
	now := time.Now()
	delta := 15 * time.Second
	var gaugeSentinel uint32

	api.SpyArgumentsAndReturnEmpty(func(args mock.Arguments) {
		query := args[1].(string)
		assert.IsType(t, v1.Range{}, args[2])
		r := args[2].(v1.Range)
		assert.Contains(t, query, "_workload=\"my_workload\"")
		assert.Contains(t, query, "_namespace=\"ns\"")
		assert.Contains(t, query, "[1m]")
		assert.NotContains(t, query, "histogram_quantile")
		atomic.AddUint32(&gaugeSentinel, 1)
		assert.Equal(t, 15*time.Second, r.Step)
		assert.WithinDuration(t, now, r.End, delta)
		assert.WithinDuration(t, now.Add(-30*time.Minute), r.Start, delta)
	})

	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}

	actual, _ := ioutil.ReadAll(resp.Body)

	assert.NotEmpty(t, actual)
	assert.Equal(t, 200, resp.StatusCode, string(actual))
	// Assert branch coverage
	assert.NotZero(t, gaugeSentinel)
}

func TestWorkloadMetricsWithParams(t *testing.T) {
	ts, api, _ := setupWorkloadMetricsEndpoint(t)
	defer ts.Close()

	req, err := http.NewRequest("GET", ts.URL+"/api/namespaces/ns/workloads/my-workload/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	q := req.URL.Query()
	q.Add("rateInterval", "5h")
	q.Add("rateFunc", "rate")
	q.Add("step", "2")
	q.Add("queryTime", "1523364075")
	q.Add("duration", "1000")
	q.Add("byLabels[]", "response_code")
	q.Add("quantiles[]", "0.5")
	q.Add("quantiles[]", "0.95")
	q.Add("filters[]", "request_count")
	q.Add("filters[]", "request_size")
	req.URL.RawQuery = q.Encode()

	queryTime := time.Unix(1523364075, 0)
	delta := 2 * time.Second
	var histogramSentinel, gaugeSentinel uint32

	api.SpyArgumentsAndReturnEmpty(func(args mock.Arguments) {
		query := args[1].(string)
		assert.IsType(t, v1.Range{}, args[2])
		r := args[2].(v1.Range)
		assert.Contains(t, query, "rate(")
		assert.Contains(t, query, "[5h]")
		if strings.Contains(query, "histogram_quantile") {
			// Histogram specific queries
			assert.Contains(t, query, " by (le,response_code)")
			assert.Contains(t, query, "istio_request_bytes")
			atomic.AddUint32(&histogramSentinel, 1)
		} else {
			assert.Contains(t, query, " by (response_code)")
			atomic.AddUint32(&gaugeSentinel, 1)
		}
		assert.Equal(t, 2*time.Second, r.Step)
		assert.WithinDuration(t, queryTime, r.End, delta)
		assert.WithinDuration(t, queryTime.Add(-1000*time.Second), r.Start, delta)
	})

	httpclient := &http.Client{}
	resp, err := httpclient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := ioutil.ReadAll(resp.Body)

	assert.NotEmpty(t, actual)
	assert.Equal(t, 200, resp.StatusCode, string(actual))
	// Assert branch coverage
	assert.NotZero(t, histogramSentinel)
	assert.NotZero(t, gaugeSentinel)
}

func TestWorkloadMetricsBadQueryTime(t *testing.T) {
	ts, api, _ := setupWorkloadMetricsEndpoint(t)
	defer ts.Close()

	req, err := http.NewRequest("GET", ts.URL+"/api/namespaces/ns/workloads/my-workload/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	q := req.URL.Query()
	q.Add("rateInterval", "5h")
	q.Add("step", "99")
	q.Add("queryTime", "abc")
	q.Add("duration", "1000")
	req.URL.RawQuery = q.Encode()

	api.SpyArgumentsAndReturnEmpty(func(args mock.Arguments) {
		// Make sure there's no client call and we fail fast
		t.Error("Unexpected call to client while having bad request")
	})

	httpclient := &http.Client{}
	resp, err := httpclient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := ioutil.ReadAll(resp.Body)

	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(actual), "cannot parse query parameter 'queryTime'")
}

func TestWorkloadMetricsBadDuration(t *testing.T) {
	ts, api, _ := setupWorkloadMetricsEndpoint(t)
	defer ts.Close()

	req, err := http.NewRequest("GET", ts.URL+"/api/namespaces/ns/workloads/my-workload/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	q := req.URL.Query()
	q.Add("rateInterval", "5h")
	q.Add("step", "99")
	q.Add("duration", "abc")
	req.URL.RawQuery = q.Encode()

	api.SpyArgumentsAndReturnEmpty(func(args mock.Arguments) {
		// Make sure there's no client call and we fail fast
		t.Error("Unexpected call to client while having bad request")
	})

	httpclient := &http.Client{}
	resp, err := httpclient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := ioutil.ReadAll(resp.Body)

	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(actual), "cannot parse query parameter 'duration'")
}

func TestWorkloadMetricsBadStep(t *testing.T) {
	ts, api, _ := setupWorkloadMetricsEndpoint(t)
	defer ts.Close()

	req, err := http.NewRequest("GET", ts.URL+"/api/namespaces/ns/workloads/my-workload/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	q := req.URL.Query()
	q.Add("rateInterval", "5h")
	q.Add("step", "abc")
	q.Add("duration", "1000")
	req.URL.RawQuery = q.Encode()

	api.SpyArgumentsAndReturnEmpty(func(args mock.Arguments) {
		// Make sure there's no client call and we fail fast
		t.Error("Unexpected call to client while having bad request")
	})

	httpclient := &http.Client{}
	resp, err := httpclient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := ioutil.ReadAll(resp.Body)

	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(actual), "cannot parse query parameter 'step'")
}

func TestWorkloadMetricsBadRateFunc(t *testing.T) {
	ts, api, _ := setupWorkloadMetricsEndpoint(t)
	defer ts.Close()

	req, err := http.NewRequest("GET", ts.URL+"/api/namespaces/ns/workloads/my-workload/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	q := req.URL.Query()
	q.Add("rateInterval", "5h")
	q.Add("rateFunc", "invalid rate func")
	req.URL.RawQuery = q.Encode()

	api.SpyArgumentsAndReturnEmpty(func(args mock.Arguments) {
		// Make sure there's no client call and we fail fast
		t.Error("Unexpected call to client while having bad request")
	})

	httpclient := &http.Client{}
	resp, err := httpclient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := ioutil.ReadAll(resp.Body)

	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(actual), "query parameter 'rateFunc' must be either 'rate' or 'irate'")
}

func TestWorkloadMetricsInaccessibleNamespace(t *testing.T) {
	ts, _, k8s := setupWorkloadMetricsEndpoint(t)
	defer ts.Close()

	url := ts.URL + "/api/namespaces/my_namespace/workloads/my_workload/metrics"

	var nsNil *osv1.Project
	k8s.On("GetProject", "my_namespace").Return(nsNil, errors.New("no privileges"))

	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	k8s.AssertCalled(t, "GetProject", "my_namespace")
}

func setupWorkloadMetricsEndpoint(t *testing.T) (*httptest.Server, *prometheustest.PromAPIMock, *kubetest.K8SClientMock) {
	config.Set(config.NewConfig())
	api := new(prometheustest.PromAPIMock)
	k8s := kubetest.NewK8SClientMock()
	prom, err := prometheus.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	prom.Inject(api)
	k8s.On("GetProject", "ns").Return(&osv1.Project{}, nil)

	mr := mux.NewRouter()
	mr.HandleFunc("/api/namespaces/{namespace}/workloads/{workload}/metrics", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			getWorkloadMetrics(w, r, func() (*prometheus.Client, error) {
				return prom, nil
			}, func() (kubernetes.IstioClientInterface, error) {
				return k8s, nil
			})
		}))

	ts := httptest.NewServer(mr)
	business.SetWithBackends(nil, prom)
	return ts, api, k8s
}
