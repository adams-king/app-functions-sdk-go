//
// Copyright (c) 2023 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package transforms

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/edgexfoundry/app-functions-sdk-go/v3/internal"
	"github.com/edgexfoundry/app-functions-sdk-go/v3/pkg/interfaces"
	"github.com/edgexfoundry/app-functions-sdk-go/v3/pkg/util"
	gometrics "github.com/rcrowley/go-metrics"

	"github.com/edgexfoundry/go-mod-core-contracts/v3/common"
)

// HTTPSender ...
type HTTPSender struct {
	url                 string
	mimeType            string
	persistOnError      bool
	continueOnSendError bool
	returnInputData     bool
	httpHeaderName      string
	secretValueKey      string
	secretName          string
	urlFormatter        StringValuesFormatter
	httpSizeMetrics     gometrics.Histogram
	httpErrorMetric     gometrics.Counter
	httpRequestHeaders  map[string]string
}

// NewHTTPSender creates, initializes and returns a new instance of HTTPSender
func NewHTTPSender(url string, mimeType string, persistOnError bool) *HTTPSender {
	return NewHTTPSenderWithOptions(HTTPSenderOptions{
		URL:            url,
		MimeType:       mimeType,
		PersistOnError: persistOnError,
	})
}

// NewHTTPSenderWithSecretHeader creates, initializes and returns a new instance of HTTPSender configured to use a secret header
func NewHTTPSenderWithSecretHeader(url string, mimeType string, persistOnError bool, headerName string, secretName string, secretValueKey string) *HTTPSender {
	return NewHTTPSenderWithOptions(HTTPSenderOptions{
		URL:            url,
		MimeType:       mimeType,
		PersistOnError: persistOnError,
		HTTPHeaderName: headerName,
		SecretName:     secretName,
		SecretValueKey: secretValueKey,
	})
}

// NewHTTPSenderWithOptions creates, initializes and returns a new instance of HTTPSender configured with provided options
func NewHTTPSenderWithOptions(options HTTPSenderOptions) *HTTPSender {
	return &HTTPSender{
		url:                 options.URL,
		mimeType:            options.MimeType,
		persistOnError:      options.PersistOnError,
		continueOnSendError: options.ContinueOnSendError,
		returnInputData:     options.ReturnInputData,
		httpHeaderName:      options.HTTPHeaderName,
		secretValueKey:      options.SecretValueKey,
		secretName:          options.SecretName,
		urlFormatter:        options.URLFormatter,
	}
}

// HTTPSenderOptions contains all options available to the sender
type HTTPSenderOptions struct {
	// URL of destination
	URL string
	// MimeType to send to destination
	MimeType string
	// PersistOnError enables use of store & forward loop if true
	PersistOnError bool
	// HTTPHeaderName to use for passing configured secret
	HTTPHeaderName string
	// SecretName is the name of the secret in the SecretStore
	SecretName string
	//  SecretValueKey is the key for the value in the secret data from the SecretStore
	SecretValueKey string
	// URLFormatter specifies custom formatting behavior to be applied to configured URL.
	// If nothing specified, default behavior is to attempt to replace placeholders in the
	// form '{some-context-key}' with the values found in the context storage.
	URLFormatter StringValuesFormatter
	// ContinueOnSendError allows execution of subsequent chained senders after errors if true
	ContinueOnSendError bool
	// ReturnInputData enables chaining multiple HTTP senders if true
	ReturnInputData bool
}

// HTTPPost will send data from the previous function to the specified Endpoint via http POST.
// If no previous function exists, then the event that triggered the pipeline will be used.
// An empty string for the mimetype will default to application/json.
func (sender *HTTPSender) HTTPPost(ctx interfaces.AppFunctionContext, data interface{}) (bool, interface{}) {
	return sender.httpSend(ctx, data, http.MethodPost)
}

// HTTPPut will send data from the previous function to the specified Endpoint via http PUT.
// If no previous function exists, then the event that triggered the pipeline will be used.
// An empty string for the mimetype will default to application/json.
func (sender *HTTPSender) HTTPPut(ctx interfaces.AppFunctionContext, data interface{}) (bool, interface{}) {
	return sender.httpSend(ctx, data, http.MethodPut)
}

func (sender *HTTPSender) httpSend(ctx interfaces.AppFunctionContext, data interface{}, method string) (bool, interface{}) {
	lc := ctx.LoggingClient()

	lc.Debugf("HTTP Exporting in pipeline '%s'", ctx.PipelineId())

	if data == nil {
		// We didn't receive a result
		return false, fmt.Errorf("function HTTP%s in pipeline '%s': No Data Received", method, ctx.PipelineId())
	}

	if sender.persistOnError && sender.continueOnSendError {
		return false, fmt.Errorf("in pipeline '%s' persistOnError & continueOnSendError can not both be set to true for HTTP Export", ctx.PipelineId())
	}

	if sender.continueOnSendError && !sender.returnInputData {
		return false, fmt.Errorf("in pipeline '%s' continueOnSendError can only be used in conjunction returnInputData for multiple HTTP Export", ctx.PipelineId())
	}

	if sender.mimeType == "" {
		sender.mimeType = "application/json"
	}

	exportData, err := util.CoerceType(data)
	if err != nil {
		return false, err
	}

	usingSecrets, err := sender.determineIfUsingSecrets(ctx)
	if err != nil {
		return false, err
	}

	formattedUrl, err := sender.urlFormatter.invoke(sender.url, ctx, data)
	if err != nil {
		return false, err
	}

	parsedUrl, err := url.Parse(formattedUrl)
	if err != nil {
		return false, err
	}

	createRegisterMetric(ctx,
		func() string { return fmt.Sprintf("%s-%s", internal.HttpExportErrorsName, parsedUrl.Redacted()) },
		func() any { return sender.httpErrorMetric },
		func() { sender.httpErrorMetric = gometrics.NewCounter() },
		map[string]string{"url": parsedUrl.Redacted()})

	createRegisterMetric(ctx,
		func() string { return fmt.Sprintf("%s-%s", internal.HttpExportSizeName, parsedUrl.Redacted()) },
		func() any { return sender.httpSizeMetrics },
		func() {
			sender.httpSizeMetrics = gometrics.NewHistogram(gometrics.NewUniformSample(internal.MetricsReservoirSize))
		},
		map[string]string{"url": parsedUrl.Redacted()})

	client := &http.Client{}
	req, err := http.NewRequest(method, parsedUrl.String(), bytes.NewReader(exportData))
	if err != nil {
		return false, err
	}
	var theSecrets map[string]string
	if usingSecrets {
		theSecrets, err = ctx.SecretProvider().GetSecret(sender.secretName, sender.secretValueKey)
		if err != nil {
			return false, err
		}

		lc.Debugf("Setting HTTP Header '%s' with secret value from SecretStore at secretName='%s' & secretKeyValue='%s in pipeline '%s'",
			sender.httpHeaderName,
			sender.secretName,
			sender.secretValueKey,
			ctx.PipelineId())

		req.Header.Set(sender.httpHeaderName, theSecrets[sender.secretValueKey])
	}

	req.Header.Set("Content-Type", sender.mimeType)

	// Set all the http request headers
	for key, element := range sender.httpRequestHeaders {
		req.Header.Set(key, element)

	}

	ctx.LoggingClient().Debugf("POSTing data to %s in pipeline '%s'", parsedUrl.Redacted(), ctx.PipelineId())

	response, err := client.Do(req)
	// Pipeline continues if we get a 2xx response, non-2xx response may stop pipeline
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		if err == nil {
			err = fmt.Errorf("export failed with %d HTTP status code in pipeline '%s'", response.StatusCode, ctx.PipelineId())
		} else {
			err = fmt.Errorf("export failed in pipeline '%s': %s", ctx.PipelineId(), err.Error())
		}

		sender.httpErrorMetric.Inc(1)

		// If continuing on send error then can't be persisting on error since Store and Forward retries starting
		// with the function that failed and stopped the execution of the pipeline.
		if !sender.continueOnSendError {
			sender.setRetryData(ctx, exportData)
			return false, err
		}

		// Continuing pipeline on error
		// This is in support of sending to multiple export destinations by chaining export functions in the pipeline.
		ctx.LoggingClient().Errorf("Continuing pipeline on error in pipeline '%s': %s", ctx.PipelineId(), err.Error())

		// Return the input data since must have some data for the next function to operate on.
		return true, data
	}

	// capture the size into metrics
	exportDataBytes := len(exportData)
	sender.httpSizeMetrics.Update(int64(exportDataBytes))

	ctx.LoggingClient().Debugf("Sent %d bytes of data in pipeline '%s'. Response status is %s", exportDataBytes, ctx.PipelineId(), response.Status)
	ctx.LoggingClient().Tracef("Data exported for pipeline '%s' (%s=%s)", ctx.PipelineId(), common.CorrelationHeader, ctx.CorrelationID())

	// This allows multiple HTTP Exports to be chained in the pipeline to send the same data to different destinations
	// Don't need to read the response data since not going to return it so just return now.
	if sender.returnInputData {
		return true, data
	}

	defer func() { _ = response.Body.Close() }()
	responseData, errReadingBody := io.ReadAll(response.Body)
	if errReadingBody != nil {
		// Can't have continueOnSendError=true when returnInputData=false, so no need to check for it here
		sender.setRetryData(ctx, exportData)
		return false, errReadingBody
	}

	return true, responseData
}

// SetHttpRequestHeaders will set all the header parameters for the http request
func (sender *HTTPSender) SetHttpRequestHeaders(httpRequestHeaders map[string]string) {

	if httpRequestHeaders != nil {
		sender.httpRequestHeaders = httpRequestHeaders
	}

}

func (sender *HTTPSender) determineIfUsingSecrets(ctx interfaces.AppFunctionContext) (bool, error) {
	// not using secrets if both are empty
	if len(sender.secretName) == 0 && len(sender.secretValueKey) == 0 {
		if len(sender.httpHeaderName) == 0 {
			return false, nil
		}

		return false, fmt.Errorf("in pipeline '%s', secretName & secretValueKey must be specified when HTTP Header Name is specified", ctx.PipelineId())
	}

	//check if one field but not others are provided for secrets
	if len(sender.secretName) != 0 && len(sender.secretValueKey) == 0 {
		return false, fmt.Errorf("in pipeline '%s', secretName was specified but no secretName was provided", ctx.PipelineId())
	}
	if len(sender.secretValueKey) != 0 && len(sender.secretName) == 0 {
		return false, fmt.Errorf("in pipeline '%s', HTTP Header secretName was provided but no secretName was provided", ctx.PipelineId())
	}

	if len(sender.httpHeaderName) == 0 {
		return false, fmt.Errorf("in pipeline '%s', HTTP Header Name required when using secrets", ctx.PipelineId())
	}

	// using secrets, all required fields are provided
	return true, nil
}

func (sender *HTTPSender) setRetryData(ctx interfaces.AppFunctionContext, exportData []byte) {
	if sender.persistOnError {
		ctx.SetRetryData(exportData)
	}
}
