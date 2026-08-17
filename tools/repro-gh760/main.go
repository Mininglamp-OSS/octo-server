// Cross-repo harness for GH#760: a fake COS that octo-web can actually talk to.
//
// Serves two things:
//
//	GET  /file/upload/credentials   — the octo-server response shape, with
//	                                  Content-Disposition built by the REAL
//	                                  file.BuildContentDisposition and the URL
//	                                  presigned by minio-go v7.0.61, exactly as
//	                                  ServiceCOS.PresignedPutURL does.
//	PUT  /<bucket>/<key>            — SigV4 verification. Two listeners: one
//	                                  applying the AWS/COS quoted-string
//	                                  Trimall rule, one applying minio-go's.
//
// Consumed by octo-web packages/dmworkdatasource/src/issue760.upload.e2e.test.ts,
// which skips unless OCTO_REPRO_GATEWAY_API points here.
//
// Fidelity limits — this is an external package, so the unexported parts of
// the handler cannot be reused. It mirrors production for the signing
// boundary only:
//
//	mirrored  — BuildContentDisposition (the real exported function), the
//	            signed header set, minio-go PresignHeader, the response shape,
//	            and the same whitespace normalization getUploadCredentials
//	            applies to contentType.
//	NOT mirrored — sanitizeFilename, the extension allowlist, fileSize bounds,
//	            auth, and the sticker keyspace guard (all unexported or
//	            request-scoped). Feed it names that are already sanitize-clean,
//	            and do not read a result here as a statement about validation.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
)

const (
	accessKey = "AKIAREPRO760EXAMPLE"
	secretKey = "wJalrXUtnFEMI-K7MDENG-bPxRfiCYEXAMPLEKEY"
	region    = "ap-guangzhou"
	bucket    = "octo-repro-1250000000"
)

// minio-go v7.0.61 pkg/signer/utils.go signV4TrimAll, verbatim.
func trimAllMinIO(in string) string { return strings.Join(strings.Fields(in), " ") }

// AWS SigV4 Trimall: collapse runs outside quoted strings only.
func trimAllQuotedAware(in string) string {
	in = strings.Trim(in, " \t")
	var b strings.Builder
	inQuotes, prevSpace := false, false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '\\' && inQuotes && i+1 < len(in) {
			b.WriteByte(c)
			b.WriteByte(in[i+1])
			i++
			prevSpace = false
			continue
		}
		if c == '"' {
			inQuotes = !inQuotes
			b.WriteByte(c)
			prevSpace = false
			continue
		}
		if !inQuotes && (c == ' ' || c == '\t') {
			if prevSpace {
				continue
			}
			prevSpace = true
			b.WriteByte(' ')
			continue
		}
		prevSpace = false
		b.WriteByte(c)
	}
	return b.String()
}

type gateway struct {
	name    string
	addr    string
	trimAll func(string) string

	mu         sync.Mutex
	lastCD     string
	lastStatus int
	lastBytes  int64
}

func (g *gateway) record(cd string, status int, n int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastCD, g.lastStatus, g.lastBytes = cd, status, n
}

func (g *gateway) snapshot() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return map[string]any{"observedContentDisposition": g.lastCD, "status": g.lastStatus, "bytes": g.lastBytes}
}

func (g *gateway) canonicalRequest(r *http.Request) string {
	q := r.URL.Query()
	signed := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	sort.Strings(signed)
	q.Del("X-Amz-Signature")

	var headers strings.Builder
	for _, h := range signed {
		var v string
		switch h {
		case "host":
			v = r.Host
		case "content-length":
			v = strconv.FormatInt(r.ContentLength, 10)
		default:
			v = r.Header.Get(h)
		}
		headers.WriteString(h + ":" + g.trimAll(v) + "\n")
	}
	return strings.Join([]string{
		r.Method,
		s3utils.EncodePath(r.URL.Path),
		strings.ReplaceAll(q.Encode(), "+", "%20"),
		headers.String(),
		strings.Join(signed, ";"),
		"UNSIGNED-PAYLOAD",
	}, "\n")
}

func (g *gateway) servePut(w http.ResponseWriter, r *http.Request) {
	n, _ := io.Copy(io.Discard, r.Body)

	q := r.URL.Query()
	parts := strings.SplitN(q.Get("X-Amz-Credential"), "/", 2)
	if len(parts) != 2 {
		http.Error(w, "InvalidCredential", http.StatusForbidden)
		return
	}
	scope := parts[1]

	sum := sha256.Sum256([]byte(g.canonicalRequest(r)))
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256", q.Get("X-Amz-Date"), scope, hex.EncodeToString(sum[:]),
	}, "\n")

	mac := func(k, d []byte) []byte {
		h := hmac.New(sha256.New, k)
		h.Write(d)
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+secretKey), []byte(strings.SplitN(scope, "/", 2)[0]))
	k = mac(k, []byte(region))
	k = mac(k, []byte("s3"))
	k = mac(k, []byte("aws4_request"))
	want := hex.EncodeToString(mac(k, []byte(sts)))

	cd := r.Header.Get("Content-Disposition")
	if hmac.Equal([]byte(want), []byte(q.Get("X-Amz-Signature"))) {
		g.record(cd, http.StatusOK, n)
		log.Printf("[%s] PUT 200  bytes=%d  cd=%q", g.name, n, cd)
		w.WriteHeader(http.StatusOK)
		return
	}
	g.record(cd, http.StatusForbidden, n)
	log.Printf("[%s] PUT 403 SignatureDoesNotMatch  bytes=%d  cd=%q", g.name, n, cd)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w,
		`<?xml version="1.0" encoding="UTF-8"?><Error><Code>SignatureDoesNotMatch</Code>`+
			`<Message>The request signature we calculated does not match the signature you provided.</Message></Error>`)
}

// serveCredentials mirrors getUploadCredentials' response shape.
func serveCredentials(w http.ResponseWriter, r *http.Request, gateways map[string]*gateway) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	q := r.URL.Query()
	filename := q.Get("filename")
	contentType := q.Get("contentType")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	fileSize, err := strconv.ParseInt(q.Get("fileSize"), 10, 64)
	if err != nil || fileSize <= 0 {
		http.Error(w, `{"msg":"fileSize 参数必须为正整数（字节）"}`, http.StatusBadRequest)
		return
	}
	target := q.Get("gateway")
	if target == "" {
		target = "cos"
	}
	gw, ok := gateways[target]
	if !ok {
		http.Error(w, `{"msg":"unknown gateway"}`, http.StatusBadRequest)
		return
	}

	// The production line under test.
	contentDisposition := file.BuildContentDisposition(filename)
	// Mirrors the normalization getUploadCredentials applies before it both
	// signs contentType and echoes it to the client (GH#760).
	contentType = strings.Join(strings.Fields(contentType), " ")

	client, err := minio.New(gw.addr, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       false,
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	key := "chat" + q.Get("path")
	headers := http.Header{}
	headers.Set("Content-Length", strconv.FormatInt(fileSize, 10))
	headers.Set("Content-Type", contentType)
	if contentDisposition != "" {
		headers.Set("Content-Disposition", contentDisposition)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	presigned, err := client.PresignHeader(ctx, http.MethodPut, bucket, key, 30*time.Minute, nil, headers)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"method":      "PUT",
		"uploadUrl":   presigned.String(),
		"downloadUrl": "http://" + gw.addr + "/" + bucket + "/" + strings.TrimPrefix(key, "/"),
		"contentType": contentType,
		"key":         key,
		"expiresIn":   1800,
		"expiredTime": time.Now().Add(30 * time.Minute).Unix(),
		"maxFileSize": fileSize,
	}
	if contentDisposition != "" {
		resp["contentDisposition"] = contentDisposition
	}
	log.Printf("[credentials] gateway=%s filename=%q cd=%q", target, filename, contentDisposition)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func listen() (net.Listener, string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	return ln, ln.Addr().String()
}

func main() {
	portsOut := flag.String("ports-file", "", "write the resolved addresses here as JSON")
	flag.Parse()

	cosLn, cosAddr := listen()
	minioLn, minioAddr := listen()
	apiLn, apiAddr := listen()

	gateways := map[string]*gateway{
		"cos":   {name: "cos", addr: cosAddr, trimAll: trimAllQuotedAware},
		"minio": {name: "minio", addr: minioAddr, trimAll: trimAllMinIO},
	}

	for _, gw := range gateways {
		g := gw
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.Header().Set("Access-Control-Allow-Methods", "PUT,GET,OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			if r.Method != http.MethodPut {
				http.Error(w, "only PUT", http.StatusMethodNotAllowed)
				return
			}
			g.servePut(w, r)
		})
		ln := cosLn
		if g.name == "minio" {
			ln = minioLn
		}
		go func() { _ = http.Serve(ln, mux) }()
	}

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/file/upload/credentials", func(w http.ResponseWriter, r *http.Request) {
		serveCredentials(w, r, gateways)
	})
	apiMux.HandleFunc("/v1/file/upload/credentials", func(w http.ResponseWriter, r *http.Request) {
		serveCredentials(w, r, gateways)
	})
	// What the client actually put on the wire, for pass-through assertions.
	apiMux.HandleFunc("/repro/last", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		name := r.URL.Query().Get("gateway")
		if name == "" {
			name = "cos"
		}
		gw, ok := gateways[name]
		if !ok {
			http.Error(w, `{"msg":"unknown gateway"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gw.snapshot())
	})
	go func() { _ = http.Serve(apiLn, apiMux) }()

	addrs := map[string]string{
		"api":   "http://" + apiAddr + "/",
		"cos":   "http://" + cosAddr,
		"minio": "http://" + minioAddr,
	}
	blob, _ := json.Marshal(addrs)
	if *portsOut != "" {
		if err := os.WriteFile(*portsOut, blob, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println(string(blob))
	log.Printf("api=%s cos=%s minio=%s", addrs["api"], addrs["cos"], addrs["minio"])
	select {}
}
