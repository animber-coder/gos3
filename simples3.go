// LICENSE MIT
// Copyright (c) 2018, Rohan Verma <hello@rohanverma.net>

package gos3

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	securityCredentialsURL = "http://169.254.169.254/latest/meta-data/iam/security-credentials/"
)

// S3 provides a wrapper around your S3 credentials.
type S3 struct {
	AccessKey string
	SecretKey string
	Region    string
	Client    *http.Client

	Token     string
	Endpoint  string
	URIFormat string
}

// DownloadInput is passed to FileUpload as a parameter.
type DownloadInput struct {
	Bucket    string
	ObjectKey string
}

// UploadInput is passed to FileUpload as a parameter.
type UploadInput struct {
	// essential fields
	Bucket      string
	ObjectKey   string
	FileName    string
	ContentType string

	// optional fields
	ContentDisposition string
	ACL                string

	Body io.ReadSeeker
}

// UploadResponse receives the following XML
// in case of success, since we set a 201 response from S3.
// Sample response:
// <PostResponse>
//     <Location>https://s3.amazonaws.com/link-to-the-file</Location>
//     <Bucket>s3-bucket</Bucket>
//     <Key>development/8614bd40-691b-4668-9241-3b342c6cf429/image.jpg</Key>
//     <ETag>"32-bit-tag"</ETag>
// </PostResponse>
type UploadResponse struct {
	Location string `xml:"Location"`
	Bucket   string `xml:"Bucket"`
	Key      string `xml:"Key"`
	ETag     string `xml:"ETag"`
}

// DeleteInput is passed to FileDelete as a parameter.
type DeleteInput struct {
	Bucket    string
	ObjectKey string
}

// IAMResponse is used by NewUsingIAM to auto
// detect the credentials
type IAMResponse struct {
	Code            string `json:"Code"`
	LastUpdated     string `json:"LastUpdated"`
	Type            string `json:"Type"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

// New returns an instance of S3.
func New(region, accessKey, secretKey string) *S3 {
	return &S3{
		Region:    region,
		AccessKey: accessKey,
		SecretKey: secretKey,

		URIFormat: "https://s3.%s.amazonaws.com/%s",
	}
}

// NewUsingIAM automatically generates an Instance of S3
// using instance metatdata.
func NewUsingIAM(region string) (*S3, error) {
	return newUsingIAMImpl(securityCredentialsURL, region)
}

func newUsingIAMImpl(baseURL, region string) (*S3, error) {
	// Get the IAM role
	resp, err := http.Get(baseURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	role, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	resp, err = http.Get(baseURL + "/" + string(role))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New(http.StatusText(resp.StatusCode))
	}

	var jsonResp IAMResponse
	jsonString, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(jsonString, &jsonResp); err != nil {
		return nil, err
	}

	return &S3{
		Region:    region,
		AccessKey: jsonResp.AccessKeyID,
		SecretKey: jsonResp.SecretAccessKey,
		Token:     jsonResp.Token,

		URIFormat: "https://s3.%s.amazonaws.com/%s",
	}, nil
}

func (s3 *S3) getClient() *http.Client {
	if s3.Client == nil {
		return http.DefaultClient
	}
	return s3.Client
}

// getURL constructs a URL for a given path, with multiple optional
// arguments as individual subfolders, based on the endpoint
// specified in s3 struct.
func (s3 *S3) getURL(path string, args ...string) (uri string) {
	if len(args) > 0 {
		path += "/" + strings.Join(args, "/")
	}
	// need to encode special characters in the path part of the URL
	encodedPath := encodePath(path)

	if len(s3.Endpoint) > 0 {
		uri = s3.Endpoint + "/" + encodedPath
	} else {
		uri = fmt.Sprintf(s3.URIFormat, s3.Region, encodedPath)
	}

	return uri
}

// SetEndpoint can be used to the set a custom endpoint for
// using an alternate instance compatible with the s3 API.
// If no protocol is included in the URI, defaults to HTTPS.
func (s3 *S3) SetEndpoint(uri string) *S3 {
	if len(uri) > 0 {
		if !strings.HasPrefix(uri, "http") {
			uri = "https://" + uri
		}
		s3.Endpoint = uri
	}
	return s3
}

// SetToken can be used to set a Temporary Security Credential token obtained from
// using an IAM role or AWS STS.
func (s3 *S3) SetToken(token string) *S3 {
	if token != "" {
		s3.Token = token
	}
	return s3
}

func detectFileSize(body io.Seeker) (int64, error) {
	pos, err := body.Seek(0, 1)
	if err != nil {
		return -1, err
	}
	defer body.Seek(pos, 0)

	n, err := body.Seek(0, 2)
	if err != nil {
		return -1, err
	}
	return n, nil
}

// SetClient can be used to set the http client to be
// used by the package. If client passed is nil,
// http.DefaultClient is used.
func (s3 *S3) SetClient(client *http.Client) *S3 {
	if client != nil {
		s3.Client = client
	} else {
		s3.Client = http.DefaultClient
	}
	return s3
}

func (s3 *S3) signRequest(req *http.Request) error {
	var (
		err error

		date = req.Header.Get("Date")
		t    = time.Now().UTC()
	)

	if date != "" {
		t, err = time.Parse(http.TimeFormat, date)
		if err != nil {
			return err
		}
	}
	req.Header.Set("Date", t.Format(amzDateISO8601TimeFormat))

	// The x-amz-content-sha256 header is required for all AWS
	// Signature Version 4 requests. It provides a hash of the
	// request payload. If there is no payload, you must provide
	// the hash of an empty string.
	emptyhash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	req.Header.Set("x-amz-content-sha256", emptyhash)

	k := s3.signKeys(t)
	h := hmac.New(sha256.New, k)

	s3.writeStringToSign(h, t, req)

	auth := bytes.NewBufferString(algorithm)
	auth.Write([]byte(" Credential=" + s3.AccessKey + "/" + s3.creds(t)))
	auth.Write([]byte{',', ' '})
	auth.Write([]byte("SignedHeaders="))
	writeHeaderList(auth, req)
	auth.Write([]byte{',', ' '})
	auth.Write([]byte("Signature=" + fmt.Sprintf("%x", h.Sum(nil))))

	req.Header.Set("Authorization", auth.String())
	return nil
}

// FileDownload makes a GET call and returns a io.ReadCloser.
// After reading the response body, ensure closing the response.
func (s3 *S3) FileDownload(u DownloadInput) (io.ReadCloser, error) {
	req, err := http.NewRequest(
		http.MethodGet, s3.getURL(u.Bucket, u.ObjectKey), nil,
	)
	if err != nil {
		return nil, err
	}

	if err := s3.signRequest(req); err != nil {
		return nil, err
	}

	res, err := s3.getClient().Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != 200 {
		return nil, fmt.Errorf("status code: %s", res.Status)
	}

	return res.Body, nil
}

// FileUpload makes a POST call with the file written as multipart
// and on successful upload, checks for 200 OK.
func (s3 *S3) FileUpload(u UploadInput) (UploadResponse, error) {
	fSize, err := detectFileSize(u.Body)
	if err != nil {
		return UploadResponse{}, err
	}
	policies, err := s3.CreateUploadPolicies(UploadConfig{
		UploadURL:          s3.getURL(u.Bucket),
		BucketName:         u.Bucket,
		ObjectKey:          u.ObjectKey,
		ContentType:        u.ContentType,
		ContentDisposition: u.ContentDisposition,
		ACL:                u.ACL,
		FileSize:           fSize,
		MetaData: map[string]string{
			"success_action_status": "201", // returns XML doc on success
		},
	})

	if err != nil {
		return UploadResponse{}, err
	}

	var b bytes.Buffer
	w := multipart.NewWriter(&b)

	for k, v := range policies.Form {
		if err = w.WriteField(k, v); err != nil {
			return UploadResponse{}, err
		}
	}

	fw, err := w.CreateFormFile("file", u.FileName)
	if err != nil {
		return UploadResponse{}, err
	}
	if _, err = io.Copy(fw, u.Body); err != nil {
		return UploadResponse{}, err
	}

	// Don't forget to close the multipart writer.
	// If you don't close it, your request will be missing the terminating boundary.
	if err := w.Close(); err != nil {
		return UploadResponse{}, err
	}

	// Now that you have a form, you can submit it to your handler.
	req, err := http.NewRequest(http.MethodPost, policies.URL, &b)
	if err != nil {
		return UploadResponse{}, err
	}
	// Don't forget to set the content type, this will contain the boundary.
	req.Header.Set("Content-Type", w.FormDataContentType())

	// Submit the request
	client := s3.getClient()
	res, err := client.Do(req)
	if err != nil {
		return UploadResponse{}, err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return UploadResponse{}, err
	}
	// Check the response
	if res.StatusCode != 201 {
		return UploadResponse{}, fmt.Errorf("status code: %s: %q", res.Status, data)
	}

	var ur UploadResponse
	xml.Unmarshal(data, &ur)
	return ur, nil
}

// FileDelete makes a DELETE call with the file written as multipart
// and on successful upload, checks for 204 No Content.
func (s3 *S3) FileDelete(u DeleteInput) error {
	req, err := http.NewRequest(
		http.MethodDelete, s3.getURL(u.Bucket, u.ObjectKey), nil,
	)
	if err != nil {
		return err
	}

	if err := s3.signRequest(req); err != nil {
		return err
	}

	// Submit the request
	client := s3.getClient()
	res, err := client.Do(req)
	if err != nil {
		return err
	}

	// Check the response
	if res.StatusCode != 204 {
		return fmt.Errorf("status code: %s", res.Status)
	}

	return nil
}

// if object matches reserved string, no need to encode them
var reservedObjectNames = regexp.MustCompile("^[a-zA-Z0-9-_.~/]+$")

// encodePath encode the strings from UTF-8 byte representations to HTML hex escape sequences
//
// This is necessary since regular url.Parse() and url.Encode() functions do not support UTF-8
// non english characters cannot be parsed due to the nature in which url.Encode() is written
//
// This function on the other hand is a direct replacement for url.Encode() technique to support
// pretty much every UTF-8 character.
// adapted from https://github.com/minio/minio-go/blob/fe1f3855b146c1b6ce4199740d317e44cf9e85c2/pkg/s3utils/utils.go#L285
func encodePath(pathName string) string {
	if reservedObjectNames.MatchString(pathName) {
		return pathName
	}
	var encodedPathname strings.Builder
	for _, s := range pathName {
		if 'A' <= s && s <= 'Z' || 'a' <= s && s <= 'z' || '0' <= s && s <= '9' { // §2.3 Unreserved characters (mark)
			encodedPathname.WriteRune(s)
			continue
		}
		switch s {
		case '-', '_', '.', '~', '/': // §2.3 Unreserved characters (mark)
			encodedPathname.WriteRune(s)
			continue
		default:
			len := utf8.RuneLen(s)
			if len < 0 {
				// if utf8 cannot convert, return the same string as is
				return pathName
			}
			u := make([]byte, len)
			utf8.EncodeRune(u, s)
			for _, r := range u {
				hex := hex.EncodeToString([]byte{r})
				encodedPathname.WriteString("%" + strings.ToUpper(hex))
			}
		}
	}
	return encodedPathname.String()
}
