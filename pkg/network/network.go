package network

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/sendgrid/rest"
)

// httpClient is a shared HTTP client with a reasonable timeout to prevent
// requests from hanging indefinitely when remote servers are unresponsive.
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

func Post(url string, body []byte, headers map[string]string) (resp *rest.Response, err error) {

	return RequestBoy(url, body, headers, rest.Post)
}

func Put(url string, body []byte, headers map[string]string) (resp *rest.Response, err error) {

	return RequestBoy(url, body, headers, rest.Put)
}

func PostForQueryParam(url string, queryParams map[string]string, headers map[string]string) (resp *rest.Response, err error) {

	return RequestBoyForQueryParam(url, queryParams, headers, rest.Post)
}

func RequestBoyForQueryParam(url string, queryParams map[string]string, headers map[string]string, method rest.Method) (resp *rest.Response, err error) {

	request := rest.Request{
		Method:      method,
		BaseURL:     url,
		QueryParams: queryParams,
		Headers:     headers,
	}
	response, err := rest.API(request)
	if err != nil {
		return nil, err
	}

	return response, nil
}
func RequestBoy(url string, body []byte, headers map[string]string, method rest.Method) (resp *rest.Response, err error) {

	request := rest.Request{
		Method:  method,
		BaseURL: url,
		Body:    body,
		Headers: headers,
	}
	response, err := rest.API(request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func Get(url string, queryParams map[string]string, headers map[string]string) (resp *rest.Response, err error) {

	request := rest.Request{
		Method:      rest.Get,
		BaseURL:     url,
		Headers:     headers,
		QueryParams: queryParams,
	}
	response, err := rest.API(request)
	if err != nil {

		return nil, err
	}

	return response, nil
}

func GetJson(url string, queryParams map[string]string, headers map[string]string) (byts []byte, err error) {

	request := rest.Request{
		Method:      rest.Get,
		BaseURL:     url,
		Headers:     headers,
		QueryParams: queryParams,
	}
	response, err := rest.API(request)
	if err != nil {

		return nil, err
	}

	return []byte(response.Body), nil
}

func PostForWWWFormForBytres(urlStr string, params map[string]string, headers map[string]string) ([]byte, error) {
	data := url.Values{}
	for key, value := range params {
		data.Set(key, value)
	}
	// Use url.Values.Encode() for proper URL encoding to prevent parameter injection
	queryStr := data.Encode()
	request, err := http.NewRequest("POST", urlStr, strings.NewReader(queryStr))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var resp *http.Response
	resp, err = httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return body, errors.New(fmt.Sprintf("状态码：%d", resp.StatusCode))
	}
	return body, nil
}

func PostForWWWForm(urlStr string, params map[string]string, headers map[string]string) (map[string]interface{}, error) {
	body, err := PostForWWWFormForBytres(urlStr, params, headers)
	if err != nil {
		return nil, err
	}
	var resultMap map[string]interface{}
	err = util.ReadJsonByByte(body, &resultMap)
	if err != nil {
		return nil, err
	}
	return resultMap, nil

}

func PostForWWWFormForAll(urlStr string, bodyData io.Reader, headers map[string]string) ([]byte, error) {
	request, err := http.NewRequest("POST", urlStr, bodyData)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var resp *http.Response
	resp, err = httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(fmt.Sprintf("状态码：%d", resp.StatusCode))
	}
	return body, nil
}

func PostForWWWFormReXML(urlStr string, params map[string]string, headers map[string]string) ([]byte, error) {
	data := url.Values{}
	for key, value := range params {
		data.Set(key, value)
	}
	// Use url.Values.Encode() for proper URL encoding to prevent parameter injection
	queryStr := data.Encode()
	request, err := http.NewRequest("POST", urlStr, strings.NewReader(queryStr))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var resp *http.Response
	resp, err = httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respData, err := io.ReadAll(resp.Body)
	// respData logged at caller if needed
	if err != nil {
		return []byte(""), err
	}
	return respData, nil

}
