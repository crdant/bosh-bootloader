package bosh_test

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/pivotal-cf-experimental/bosh-test/bosh"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("UploadStemcell", func() {
	var server *httptest.Server
	BeforeEach(func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			switch req.URL.Path {
			case "/stemcells":
				Expect(req.Method).To(Equal("POST"))
				username, password, ok := req.BasicAuth()
				Expect(ok).To(BeTrue())
				Expect(username).To(Equal("some-user"))
				Expect(password).To(Equal("some-password"))
				Expect(req.Header.Get("Content-Type")).To(Equal("application/x-compressed"))
				Expect(req.ContentLength).To(BeNumerically("==", len("I am an apple")))

				contents, err := ioutil.ReadAll(req.Body)
				Expect(err).NotTo(HaveOccurred())
				Expect(contents).To(Equal([]byte("I am an apple")))

				w.Header().Set("Location", fmt.Sprintf("http://%s/tasks/8", req.Host))
				w.WriteHeader(http.StatusFound)

			case "/tasks/8":
				Expect(req.Method).To(Equal("GET"))
				username, password, ok := req.BasicAuth()
				Expect(ok).To(BeTrue())
				Expect(username).To(Equal("some-user"))
				Expect(password).To(Equal("some-password"))

				w.Write([]byte(`{"id": 8, "state": "done"}`))

			default:
				Fail(fmt.Sprintf("unhandled request to %s", req.URL.Path))
			}
		}))
	})

	It("uploads the stemcell to the director", func() {
		client := bosh.NewClient(bosh.Config{
			URL:      server.URL,
			Username: "some-user",
			Password: "some-password",
		})

		taskID, err := client.UploadStemcell(sizeReader{strings.NewReader("I am an apple"), int64(len("I am an apple"))})
		Expect(err).NotTo(HaveOccurred())
		Expect(taskID).To(Equal(8))
	})

	Context("failure cases", func() {
		Context("when the request cannot be created", func() {
			It("returns an error", func() {
				client := bosh.NewClient(bosh.Config{
					URL: "%%%%%",
				})

				_, err := client.UploadStemcell(strings.NewReader("I am a banana!"))
				Expect(err).To(MatchError(ContainSubstring("invalid URL escape")))
			})
		})

		Context("when the request cannot be made", func() {
			It("returns an error", func() {
				client := bosh.NewClient(bosh.Config{
					URL: "",
				})

				_, err := client.UploadStemcell(strings.NewReader("I am a banana!"))
				Expect(err).To(MatchError(ContainSubstring("unsupported protocol scheme")))
			})
		})

		Context("when the request returns an unexpected response status", func() {
			It("returns an error with the body", func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					w.WriteHeader(http.StatusTeapot)
					w.Write([]byte("More Info"))
				}))

				client := bosh.NewClient(bosh.Config{
					URL: server.URL,
				})

				_, err := client.UploadStemcell(strings.NewReader("Hi"))
				Expect(err).To(MatchError("unexpected response 418 I'm a teapot:\nMore Info"))
			})
		})

		Context("when the response body cannot be read", func() {
			It("returns an error", func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					w.WriteHeader(http.StatusTeapot)
					w.Write([]byte("More Info"))
				}))

				client := bosh.NewClient(bosh.Config{
					URL: server.URL,
				})

				bosh.SetBodyReader(func(io.Reader) ([]byte, error) {
					return nil, errors.New("a bad read happened")
				})

				_, err := client.UploadStemcell(strings.NewReader("Hi"))

				Expect(err).To(MatchError("a bad read happened"))
			})
		})
	})
})
