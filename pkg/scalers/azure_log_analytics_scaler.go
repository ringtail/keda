package scalers

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	v2beta2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/metrics/pkg/apis/external_metrics"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kedautil "github.com/kedacore/keda/pkg/util"
)

const (
	miEndpoint       = "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fapi.loganalytics.io%2F"
	aadTokenEndpoint = "https://login.microsoftonline.com/%s/oauth2/token"
	laQueryEndpoint  = "https://api.loganalytics.io/v1/workspaces/%s/query"
)

type azureLogAnalyticsScaler struct {
	metadata  *azureLogAnalyticsMetadata
	cache     *sessionCache
	name      string
	namespace string
}

type azureLogAnalyticsMetadata struct {
	tenantID     string
	clientID     string
	clientSecret string
	workspaceID  string
	podIdentity  string
	query        string
	threshold    int64
}

type sessionCache struct {
	metricValue     int64
	metricThreshold int64
}

type tokenData struct {
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in,string"`
	ExtExpiresIn int    `json:"ext_expires_in,string"`
	ExpiresOn    int64  `json:"expires_on,string"`
	NotBefore    int64  `json:"not_before,string"`
	Resource     string `json:"resource"`
	AccessToken  string `json:"access_token"`
}

type metricsData struct {
	value     int64
	threshold int64
}

type queryResult struct {
	Tables []struct {
		Name    string `json:"name"`
		Columns []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"columns"`
		Rows [][]interface{} `json:"rows"`
	} `json:"tables"`
}

var tokenCache = struct {
	sync.RWMutex
	m map[string]tokenData
}{m: make(map[string]tokenData)}

var logAnalyticsLog = logf.Log.WithName("azure_log_analytics_scaler")

// NewAzureLogAnalyticsScaler creates a new Azure Log Analytics Scaler
func NewAzureLogAnalyticsScaler(resolvedSecrets, metadata, authParams map[string]string, podIdentity string, name string, namespace string) (Scaler, error) {
	azureLogAnalyticsMetadata, err := parseAzureLogAnalyticsMetadata(resolvedSecrets, metadata, authParams, podIdentity)
	if err != nil {
		return nil, fmt.Errorf("Failed to initialize Log Analytics scaler. Scaled object: %s. Namespace: %s. Inner Error: %v", name, namespace, err)
	}

	return &azureLogAnalyticsScaler{
		metadata:  azureLogAnalyticsMetadata,
		cache:     &sessionCache{metricValue: -1, metricThreshold: -1},
		name:      name,
		namespace: namespace,
	}, nil
}

func parseAzureLogAnalyticsMetadata(resolvedEnv, metadata, authParams map[string]string, podIdentity string) (*azureLogAnalyticsMetadata, error) {
	meta := azureLogAnalyticsMetadata{}

	if podIdentity == "" || podIdentity == "none" {
		//Getting tenantId
		if val, ok := authParams["tenantId"]; ok && val != "" {
			meta.tenantID = val
		} else if val, ok := metadata["tenantId"]; ok && val != "" {
			meta.tenantID = val
		} else if val, ok := metadata["tenantIdFromEnv"]; ok && val != "" {
			meta.tenantID = resolvedEnv[metadata["tenantIdFromEnv"]]
		} else {
			return nil, fmt.Errorf("Error parsing metadata. Details: tenantId was not found in metadata. Check your ScaledObject configuration")
		}

		//Getting clientId
		if val, ok := authParams["clientId"]; ok && val != "" {
			meta.clientID = val
		} else if val, ok := metadata["clientId"]; ok && val != "" {
			meta.clientID = val
		} else if val, ok := metadata["clientIdFromEnv"]; ok && val != "" {
			meta.clientID = resolvedEnv[metadata["clientIdFromEnv"]]
		} else {
			return nil, fmt.Errorf("Error parsing metadata. Details: clientId was not found in metadata. Check your ScaledObject configuration")
		}

		//Getting clientSecret
		if val, ok := authParams["clientSecret"]; ok && val != "" {
			meta.clientSecret = val
		} else if val, ok := metadata["clientSecret"]; ok && val != "" {
			meta.clientSecret = val
		} else if val, ok := metadata["clientSecretFromEnv"]; ok && val != "" {
			meta.clientSecret = resolvedEnv[metadata["clientSecretFromEnv"]]
		} else {
			return nil, fmt.Errorf("Error parsing metadata. Details: clientSecret was not found in metadata. Check your ScaledObject configuration")
		}

		meta.podIdentity = ""
	} else if podIdentity == "azure" {
		meta.podIdentity = podIdentity
	} else {
		return nil, fmt.Errorf("Error parsing metadata. Details: Log Analytics Scaler doesn't support pod identity %s", podIdentity)
	}

	//Getting workspaceId
	if val, ok := authParams["workspaceId"]; ok && val != "" {
		meta.workspaceID = val
	} else if val, ok := metadata["workspaceId"]; ok && val != "" {
		meta.workspaceID = val
	} else if val, ok := metadata["workspaceIdFromEnv"]; ok && val != "" {
		meta.workspaceID = resolvedEnv[metadata["workspaceIdFromEnv"]]
	} else {
		return nil, fmt.Errorf("Error parsing metadata. Details: workspaceId was not found in metadata. Check your ScaledObject configuration")
	}

	//Getting query
	if val, ok := metadata["query"]; ok && val != "" {
		meta.query = val
	} else if val, ok := metadata["queryFromEnv"]; ok && val != "" {
		meta.query = resolvedEnv[metadata["queryFromEnv"]]
	} else {
		return nil, fmt.Errorf("Error parsing metadata. Details: query was not found in metadata. Check your ScaledObject configuration")
	}

	//Getting threshold
	if val, ok := metadata["threshold"]; ok && val != "" {
		threshold, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Error parsing metadata. Details: can't parse threshold. Inner Error: %v", err)
		}
		meta.threshold = threshold
	} else if val, ok := metadata["thresholdFromEnv"]; ok && val != "" {
		threshold, err := strconv.ParseInt(resolvedEnv[metadata["thresholdFromEnv"]], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Error parsing metadata. Details: can't parse threshold. Inner Error: %v", err)
		}
		meta.threshold = threshold
	} else {
		return nil, fmt.Errorf("Error parsing metadata. Details: threshold was not found in metadata. Check your ScaledObject configuration")
	}

	return &meta, nil
}

// IsActive determines if we need to scale from zero
func (s *azureLogAnalyticsScaler) IsActive(ctx context.Context) (bool, error) {
	err := s.updateCache()

	if err != nil {
		return false, fmt.Errorf("Failed to execute IsActive function. Scaled object: %s. Namespace: %s. Inner Error: %v", s.name, s.namespace, err)
	}

	return s.cache.metricValue > 0, nil
}

func (s *azureLogAnalyticsScaler) GetMetricSpecForScaling() []v2beta2.MetricSpec {
	err := s.updateCache()

	if err != nil {
		logAnalyticsLog.V(1).Info("Failed to get metric spec.", "Scaled object", s.name, "Namespace", s.namespace, "Inner Error", err)
		return nil
	}

	externalMetric := &v2beta2.ExternalMetricSource{
		Metric: v2beta2.MetricIdentifier{
			Name: kedautil.NormalizeString(fmt.Sprintf("%s-%s", "azure-log-analytics", s.metadata.workspaceID)),
		},
		Target: v2beta2.MetricTarget{
			Type:         v2beta2.AverageValueMetricType,
			AverageValue: resource.NewQuantity(s.cache.metricThreshold, resource.DecimalSI),
		},
	}
	metricSpec := v2beta2.MetricSpec{External: externalMetric, Type: externalMetricType}
	return []v2beta2.MetricSpec{metricSpec}
}

//GetMetrics returns value for a supported metric and an error if there is a problem getting the metric
func (s *azureLogAnalyticsScaler) GetMetrics(ctx context.Context, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	receivedMetric, err := s.getMetricData()

	if err != nil {
		return []external_metrics.ExternalMetricValue{}, fmt.Errorf("Failed to get metrics. Scaled object: %s. Namespace: %s. Inner Error: %v", s.name, s.namespace, err)
	}

	metric := external_metrics.ExternalMetricValue{
		MetricName: metricName,
		Value:      *resource.NewQuantity(receivedMetric.value, resource.DecimalSI),
		Timestamp:  metav1.Now(),
	}

	return append([]external_metrics.ExternalMetricValue{}, metric), nil
}

func (s *azureLogAnalyticsScaler) Close() error {
	return nil
}

func (s *azureLogAnalyticsScaler) updateCache() error {
	if s.cache.metricValue < 0 {
		receivedMetric, err := s.getMetricData()

		if err != nil {
			return err
		}

		s.cache.metricValue = receivedMetric.value

		if receivedMetric.threshold > 0 {
			s.cache.metricThreshold = receivedMetric.threshold
		} else {
			s.cache.metricThreshold = s.metadata.threshold
		}
	}

	return nil
}

func (s *azureLogAnalyticsScaler) getMetricData() (metricsData, error) {
	tokenInfo, err := s.getAccessToken()
	if err != nil {
		return metricsData{}, err
	}

	metricsInfo, err := s.executeQuery(s.metadata.query, tokenInfo)
	if err != nil {
		return metricsData{}, err
	}

	logAnalyticsLog.V(1).Info("Providing metric value", "metrics value", metricsInfo.value, "scaler name", s.name, "namespace", s.namespace)

	return metricsInfo, nil
}

func (s *azureLogAnalyticsScaler) getAccessToken() (tokenData, error) {
	//if there is no token yet or it will be expired in less, that 30 secs
	currentTimeSec := time.Now().Unix()
	tokenInfo := tokenData{}

	if s.metadata.podIdentity == "" {
		tokenInfo, _ = getTokenFromCache(s.metadata.clientID, s.metadata.clientSecret)
	} else {
		tokenInfo, _ = getTokenFromCache(s.metadata.podIdentity, s.metadata.podIdentity)
	}

	if currentTimeSec+30 > tokenInfo.ExpiresOn {
		newTokenInfo, err := s.refreshAccessToken()
		if err != nil {
			return tokenData{}, err
		}

		if s.metadata.podIdentity == "" {
			logAnalyticsLog.V(1).Info("Token for Service Principal has been refreshed", "clientID", s.metadata.clientID, "scaler name", s.name, "namespace", s.namespace)
			_ = setTokenInCache(s.metadata.clientID, s.metadata.clientSecret, newTokenInfo)
		} else {
			logAnalyticsLog.V(1).Info("Token for Pod Identity has been refreshed", "type", s.metadata.podIdentity, "scaler name", s.name, "namespace", s.namespace)
			_ = setTokenInCache(s.metadata.podIdentity, s.metadata.podIdentity, newTokenInfo)
		}

		return newTokenInfo, nil
	}
	return tokenInfo, nil
}

func (s *azureLogAnalyticsScaler) executeQuery(query string, tokenInfo tokenData) (metricsData, error) {
	queryData := queryResult{}

	body, statusCode, err := s.executeLogAnalyticsREST(query, tokenInfo)

	//Handle expired token
	if statusCode == 403 || (len(body) > 0 && strings.Contains(string(body), "TokenExpired")) {
		tokenInfo, err := s.refreshAccessToken()

		if s.metadata.podIdentity == "" {
			logAnalyticsLog.V(1).Info("Token for Service Principal has been refreshed", "clientID", s.metadata.clientID, "scaler name", s.name, "namespace", s.namespace)
			_ = setTokenInCache(s.metadata.clientID, s.metadata.clientSecret, tokenInfo)
		} else {
			logAnalyticsLog.V(1).Info("Token for Pod Identity has been refreshed", "type", s.metadata.podIdentity, "scaler name", s.name, "namespace", s.namespace)
			_ = setTokenInCache(s.metadata.podIdentity, s.metadata.podIdentity, tokenInfo)
		}

		if err == nil {
			body, statusCode, err = s.executeLogAnalyticsREST(query, tokenInfo)
		} else {
			return metricsData{}, err
		}
	}

	if statusCode != 200 && statusCode != 0 {
		return metricsData{}, fmt.Errorf("Error processing Log Analytics request. HTTP code %d. Inner Error: %v. Body: %s", statusCode, err, string(body))
	}

	if err != nil {
		return metricsData{}, err
	}

	if len(body) == 0 {
		return metricsData{}, fmt.Errorf("Error processing Log Analytics request. Details: empty body. HTTP code: %d", statusCode)
	}

	err = json.NewDecoder(bytes.NewReader(body)).Decode(&queryData)
	if err != nil {
		return metricsData{}, fmt.Errorf("Error processing Log Analytics request. Details: can't decode response body to JSON from REST API result. HTTP code: %d. Inner Error: %v. Body: %s", statusCode, err, string(body))
	}

	if statusCode == 200 {
		metricsInfo := metricsData{}
		metricsInfo.threshold = s.metadata.threshold
		metricsInfo.value = 0

		//Pre-validation of query result:
		if len(queryData.Tables) == 0 || len(queryData.Tables[0].Columns) == 0 || len(queryData.Tables[0].Rows) == 0 {
			return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: there is no results after running your query. HTTP code: %d. Body: %s", statusCode, string(body))
		} else if len(queryData.Tables) > 1 {
			return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: too many tables in query result: %d, expected: 1. HTTP code: %d. Body: %s", len(queryData.Tables), statusCode, string(body))
		} else if len(queryData.Tables[0].Rows) > 1 {
			return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: too many rows in query result: %d, expected: 1. HTTP code: %d. Body: %s", len(queryData.Tables[0].Rows), statusCode, string(body))
		}

		if len(queryData.Tables[0].Rows[0]) > 0 {
			metricDataType := queryData.Tables[0].Columns[0].Type
			metricVal := queryData.Tables[0].Rows[0][0]

			if metricVal != nil {
				//type can be: real, int, long
				if metricDataType == "real" || metricDataType == "int" || metricDataType == "long" {
					metricValue, isConverted := metricVal.(float64)
					if !isConverted {
						return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: can not convert result to type float64. HTTP code: %d. Body: %s", statusCode, string(body))
					}
					if metricValue < 0 {
						return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: metric value should be >=0, but received %f. HTTP code: %d. Body: %s", metricValue, statusCode, string(body))
					}
					metricsInfo.value = int64(metricValue)
				} else {
					return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: metric value data type should be real, int or long, but received %s. HTTP code: %d Body: %s", metricDataType, statusCode, string(body))
				}
			}
		}

		if len(queryData.Tables[0].Rows[0]) > 1 {
			thresholdDataType := queryData.Tables[0].Columns[1].Type
			thresholdVal := queryData.Tables[0].Rows[0][1]

			if thresholdVal != nil {
				//type can be: real, int, long
				if thresholdDataType == "real" || thresholdDataType == "int" || thresholdDataType == "long" {
					thresholdValue, isConverted := thresholdVal.(float64)
					if !isConverted {
						return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: cannot convert threshold result to type float64. HTTP code: %d. Body: %s", statusCode, string(body))
					}
					if thresholdValue < 0 {
						return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: threshold value should be >=0, but received %f. HTTP code: %d. Body: %s", thresholdValue, statusCode, string(body))
					}
					metricsInfo.threshold = int64(thresholdValue)
				} else {
					return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: threshold value data type should be real, int or long, but received %s. HTTP code: %d. Body: %s", thresholdDataType, statusCode, string(body))
				}
			} else {
				return metricsData{}, fmt.Errorf("Error validating Log Analytics request. Details: threshold value is empty, check your query. HTTP code: %d. Body: %s", statusCode, string(body))
			}
		} else {
			metricsInfo.threshold = -1
		}

		return metricsInfo, nil
	}

	return metricsData{}, fmt.Errorf("Error processing Log Analytics request. Details: unknown error. HTTP code: %d. Body: %s", statusCode, string(body))
}

func (s *azureLogAnalyticsScaler) refreshAccessToken() (tokenData, error) {
	tokenInfo, err := s.getAuthorizationToken()

	if err != nil {
		return tokenData{}, err
	}

	//Now, let's check we can use this token. If no, wait until we can use it
	currentTimeSec := time.Now().Unix()
	if currentTimeSec < tokenInfo.NotBefore {
		if currentTimeSec < tokenInfo.NotBefore+10 {
			sleepDurationSec := int(tokenInfo.NotBefore - currentTimeSec + 1)
			logAnalyticsLog.V(1).Info("AAD token not ready", "delay (seconds)", sleepDurationSec, "scaler name", s.name, "namespace", s.namespace)
			time.Sleep(time.Duration(sleepDurationSec) * time.Second)
		} else {
			return tokenData{}, fmt.Errorf("Error getting access token. Details: AAD token has been received, but start date begins in %d seconds, so current operation will be skipped", tokenInfo.NotBefore-currentTimeSec)
		}
	}

	return tokenInfo, nil
}

func (s *azureLogAnalyticsScaler) getAuthorizationToken() (tokenData, error) {
	body, statusCode, err, tokenInfo := []byte{}, 0, *new(error), tokenData{}
	if s.metadata.podIdentity == "" {
		body, statusCode, err = s.executeAADApicall()
	} else {
		body, statusCode, err = s.executeIMDSApicall()
	}

	if err != nil {
		return tokenData{}, fmt.Errorf("Error getting access token. HTTP code: %d. Inner Error: %v. Body: %s", statusCode, err, string(body))
	} else if len(body) == 0 {
		return tokenData{}, fmt.Errorf("Error getting access token. Details: empty body. HTTP code: %d", statusCode)
	}

	err = json.NewDecoder(bytes.NewReader(body)).Decode(&tokenInfo)
	if err != nil {
		return tokenData{}, fmt.Errorf("Error getting access token. Details: can't decode response body to JSON after getting access token. HTTP code: %d. Inner Error: %v. Body: %s", statusCode, err, string(body))
	}

	if statusCode == 200 {
		return tokenInfo, nil
	}

	return tokenData{}, fmt.Errorf("Error getting access token. Details: unknown error. HTTP code: %d. Body: %s", statusCode, string(body))
}

func (s *azureLogAnalyticsScaler) executeLogAnalyticsREST(query string, tokenInfo tokenData) ([]byte, int, error) {
	m := map[string]interface{}{"query": query}

	jsonBytes, err := json.Marshal(m)
	if err != nil {
		return nil, 0, fmt.Errorf("Can't construct JSON for request to Log Analytics API. Inner Error: %v", err)
	}

	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf(laQueryEndpoint, s.metadata.workspaceID), bytes.NewBuffer(jsonBytes)) // URL-encoded payload
	if err != nil {
		return nil, 0, fmt.Errorf("Can't construct HTTP request to Log Analytics API. Inner Error: %v", err)
	}

	request.Header.Add("Content-Type", "application/json")
	request.Header.Add("Authorization", fmt.Sprintf("Bearer %s", tokenInfo.AccessToken))
	request.Header.Add("Content-Length", fmt.Sprintf("%d", len(jsonBytes)))

	return s.runHTTP(request, "Log Analytics REST api")
}

func (s *azureLogAnalyticsScaler) executeAADApicall() ([]byte, int, error) {
	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {s.metadata.clientID},
		"redirect_uri":  {"http://"},
		"resource":      {"https://api.loganalytics.io/"},
		"client_secret": {s.metadata.clientSecret},
	}

	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf(aadTokenEndpoint, s.metadata.tenantID), strings.NewReader(data.Encode())) // URL-encoded payload
	if err != nil {
		return nil, 0, fmt.Errorf("Can't construct HTTP request to Azure Active Directory. Inner Error: %v", err)
	}

	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Add("Content-Length", fmt.Sprintf("%d", len(data.Encode())))

	return s.runHTTP(request, "AAD")
}

func (s *azureLogAnalyticsScaler) executeIMDSApicall() ([]byte, int, error) {
	request, err := http.NewRequest(http.MethodGet, miEndpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("Can't construct HTTP request to Azure Instance Metadata service. Inner Error: %v", err)
	}

	request.Header.Add("Metadata", "true")

	return s.runHTTP(request, "IMDS")
}

func (s *azureLogAnalyticsScaler) runHTTP(request *http.Request, caller string) ([]byte, int, error) {
	request.Header.Add("Cache-Control", "no-cache")
	request.Header.Add("User-Agent", "keda/2.0.0")

	httpClient := &http.Client{}

	resp, err := httpClient.Do(request)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("Error calling %s. Inner Error: %v", caller, err)
	}

	defer resp.Body.Close()
	httpClient.CloseIdleConnections()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("Error reading %s response body: Inner Error: %v", caller, err)
	}

	return body, resp.StatusCode, nil
}

func getTokenFromCache(clientID string, clientSecret string) (tokenData, error) {
	key, err := getHash(clientID, clientSecret)
	if err != nil {
		return tokenData{}, fmt.Errorf("Error calculating sha1 hash. Inner Error: %v", err)
	}

	tokenCache.RLock()

	if val, ok := tokenCache.m[key]; ok && val.AccessToken != "" {
		tokenCache.RUnlock()
		return val, nil
	}

	tokenCache.RUnlock()
	return tokenData{}, fmt.Errorf("Error getting value from token cache. Details: unknown error")
}

func setTokenInCache(clientID string, clientSecret string, tokenInfo tokenData) error {
	key, err := getHash(clientID, clientSecret)
	if err != nil {
		return err
	}

	tokenCache.Lock()
	tokenCache.m[key] = tokenInfo
	tokenCache.Unlock()

	return nil
}

func getHash(clientID string, clientSecret string) (string, error) {
	sha1Hash := sha1.New()
	_, err := sha1Hash.Write([]byte(fmt.Sprintf("%s|%s", clientID, clientSecret)))

	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(sha1Hash.Sum(nil)), nil
}
