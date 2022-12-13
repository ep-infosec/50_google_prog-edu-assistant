// Command uploadserver starts the upload server.
package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/golang/glog"
	"github.com/google/prog-edu-assistant/autograder"
	"github.com/google/prog-edu-assistant/queue"
	"github.com/google/prog-edu-assistant/uploadserver"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var (
	port             = flag.Int("port", 0, "The port to serve HTTP/S. If 0, use the PORT environment variable, or 8000 if PORT is unset.")
	useHTTPS         = flag.Bool("use_https", false, "If true, use HTTPS instead of HTTP.")
	secureCookie     = flag.Bool("secure_cookie", false, "If true, set Secure attribute on cookies even with http. Secure cookie is always set with https.")
	httpRedirectPort = flag.Int("http_redirect_port", 0, "If non-zero, listen HTTP on the specified port and redirect to to SERVER_URL (assumed to be HTTPS)")
	sslCertFile      = flag.String("ssl_cert_file", "localhost.crt",
		"The path to the signed SSL server certificate.")
	sslKeyFile = flag.String("ssl_key_file", "localhost.key",
		"The path to the SSL server key.")
	allowCORS = flag.Bool("allow_cors", false,
		"If true, allow cross-origin requests from any domain."+
			"This is currently necessary to enable uploads from Jupyter notebooks, "+
			"but unfortunately "+
			"it also makes the server vulnerable to XSRF attacks. Use with care.")
	useOpenID = flag.Bool("use_openid", false, "If true, use OpenID Connect authentication"+
		" provided by the issuer specified with --openid_issuer.")
	openIDIssuer = flag.String("openid_issuer", "https://accounts.google.com",
		"The URL of the OpenID Connect issuer. "+
			"/.well-known/openid-configuration will be "+
			"requested for detailed endpoint configuration. Defaults to Google.")
	allowedUsersFile = flag.String("allowed_users_file", "",
		"The file name of a text file with one user email per line. If not specified, only authentication "+
			"is performed without authorization.")
	uploadDir = flag.String("upload_dir", "uploads", "The directory to write uploaded notebooks.")
	queueSpec = flag.String("queue_spec", "amqp://guest:guest@localhost:5672/",
		"The spec of the queue to connect to.")
	autograderQueue = flag.String("autograder_queue", "autograde",
		"The name of the autograder queue to send work requests.")
	reportQueue = flag.String("report_queue", "report",
		"The name of the queue to listen for the reports.")
	staticDir = flag.String("static_dir", "", "The directory to serve static files from. "+
		"The files are exposed at the path /static.")
	gradeLocally = flag.Bool("grade_locally", false,
		"If true, specifies that the server should run the autograder locally "+
			"instead of using the message queue.")
	autograderDir = flag.String("autograder_dir", "",
		"The root directory of autograder scripts. Used with --grade_locally.")
	nsjailPath = flag.String("nsjail_path", "/usr/local/bin/nsjail",
		"The path to nsjail binary. Used with --grade_locally.")
	pythonPath = flag.String("python_path", "/usr/bin/python3",
		"The path to python binary. Used with --grade_locally.")
	scratchDir = flag.String("scratch_dir", "/tmp/autograde",
		"The base directory to create scratch directories for autograding. "+
			"Used with --grade_locally.")
	disableCleanup = flag.Bool("disable_cleanup", false,
		"If true, does not delete scratch directory after running the tests. "+
			"Used with --grade_locally.")
	autoRemove = flag.Bool("auto_remove", false,
		"If true, removes the scratch directory before creating a new one. "+
			"This is useful together with --disable_cleanup and --grade_locally.")
	includeLogsToReport = flag.Bool("include_logs_to_report", false,
		"If true, autograder includes the low-level output of nsjail into report. "+
			"This is useful for debugging.")

	logToBucket = flag.Bool("log_to_bucket", false,
		"If true, configures the server to write logs to Google Cloud "+
			"Storage bucket. The bucket name should be provided "+
			"in the environment variable LOG_BUCKET, "+
			"and the GCP project ID should be provided in "+
			"the environment variable GCP_PROJECT")
	useJWT = flag.Bool("use_jwt", true,
		"If true, configures the server to support bearer authorization with JWT, "+
			"as well as server handler to issue authorization tokens. If this is enabled, "+
			"the key is read from a file stored in a cloud bucket and named in the format "+
			"gs://bucket/keyfile in the environment variable JWT_KEY.")
)

func main() {
	flag.Parse()
	glog.Infof("Starting server: %q", os.Args)
	err := run()
	if err != nil {
		glog.Exit(err)
	}
}

func run() error {
	endpoint := google.Endpoint
	userinfoEndpoint := "https://openidconnect.googleapis.com/v1/userinfo"
	if *useOpenID && *openIDIssuer != "" {
		wellKnownURL := *openIDIssuer + "/.well-known/openid-configuration"
		resp, err := http.Get(wellKnownURL)
		if err != nil {
			return fmt.Errorf("Error on GET %s: %s", wellKnownURL, err)
		}
		defer resp.Body.Close()
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		data := make(map[string]interface{})
		err = json.Unmarshal(b, &data)
		if err != nil {
			return fmt.Errorf("Error parsing response from %s: %s", wellKnownURL, err)
		}
		// Override the authentication endpoint.
		auth_ep, ok := data["authorization_endpoint"].(string)
		if !ok {
			return fmt.Errorf("response from %s does not have 'authorization_endpoint' key", wellKnownURL)
		}
		token_ep, ok := data["token_endpoint"].(string)
		if !ok {
			return fmt.Errorf("response from %s does not have 'token_endpoint' key", wellKnownURL)
		}
		endpoint = oauth2.Endpoint{
			AuthURL:   auth_ep,
			TokenURL:  token_ep,
			AuthStyle: oauth2.AuthStyleInParams,
		}
		glog.Infof("auth endpoint: %#v", endpoint)
		userinfoEndpoint, ok = data["userinfo_endpoint"].(string)
		if !ok {
			return fmt.Errorf("response from %s does not have 'userinfo_endpoint' key", wellKnownURL)
		}
		glog.Infof("userinfo endpoint: %#v", userinfoEndpoint)
	}
	allowedUsers := make(map[string]bool)
	if *allowedUsersFile != "" {
		b, err := ioutil.ReadFile(*allowedUsersFile)
		if err != nil {
			return fmt.Errorf("error reading --allowed_users_file %q: %s", *allowedUsersFile, err)
		}
		for _, email := range strings.Split(string(b), "\n") {
			if email == "" {
				continue
			}
			allowedUsers[email] = true
		}
	}
	var q *queue.Channel
	var ch <-chan []byte
	var ag *autograder.Autograder
	if *gradeLocally {
		ag = &autograder.Autograder{
			Dir:            *autograderDir,
			ScratchDir:     *scratchDir,
			NSJailPath:     *nsjailPath,
			PythonPath:     *pythonPath,
			DisableCleanup: *disableCleanup,
			AutoRemove:     *autoRemove,
			IncludeLogs:    *includeLogsToReport,
		}
	} else {
		// Connect to message queue if not grading locally.
		delay := 500 * time.Millisecond
		retryUntil := time.Now().Add(60 * time.Second)
		for {
			var err error
			q, err = queue.Open(*queueSpec)
			if err != nil {
				if time.Now().After(retryUntil) {
					return fmt.Errorf("error opening queue %q: %s", *queueSpec, err)
				}
				glog.V(1).Infof("error opening queue %q: %s, retrying in %s", *queueSpec, err, delay)
				time.Sleep(delay)
				delay = delay * 2
				continue
			}
			ch, err = q.Receive(*reportQueue)
			if err != nil {
				return fmt.Errorf("error receiving on queue %q: %s", *reportQueue, err)
			}
			glog.Infof("Listening for reports on the queue %q", *reportQueue)
			break
		}
	}
	addr := ":" + strconv.Itoa(*port)
	if *port == 0 {
		envValue := os.Getenv("PORT")
		if envValue == "" {
			addr = ":8000"
		} else {
			_, err := strconv.ParseInt(envValue, 10, 32)
			if err != nil {
				return fmt.Errorf("error parsing PORT value %q: %s", envValue, err)
			}
			addr = ":" + envValue
		}
	}
	protocol := "http"
	if *useHTTPS {
		protocol = "https"
	}
	serverURL := fmt.Sprintf("%s://localhost%s", protocol, addr)
	if os.Getenv("SERVER_URL") != "" {
		// Allow override from the environment.
		serverURL = os.Getenv("SERVER_URL")
		glog.Info("Environment provided override SERVER_URL=%s", serverURL)
	}
	var rsaKey *rsa.PrivateKey
	if *useJWT {
		jwtKey := os.Getenv("JWT_KEY")
		if jwtKey == "" {
			return fmt.Errorf("need to set JWT_KEY in order to use JWT authentication")
		}
		var b []byte
		glog.Infof("JWT_KEY = %q", jwtKey)
		var err error
		if strings.HasPrefix(jwtKey, "gs://") {
			ctx := context.Background()
			client, err := storage.NewClient(ctx)
			if err != nil {
				return fmt.Errorf("error creating Cloud Storage client: %s", err)
			}
			// Load the key from Cloud Storage.
			parts := strings.SplitN(jwtKey, "/", 4)
			if len(parts) != 4 || parts[0] != "gs:" || parts[1] != "" {
				return fmt.Errorf("JWT_KEY must have gs://bucket/keyfile format, got %q", jwtKey)
			}
			bucket := client.Bucket(parts[2])
			obj := bucket.Object(parts[3])
			reader, err := obj.NewReader(ctx)
			if err != nil {
				return fmt.Errorf("error reading from bucket object %q: %s", jwtKey, err)
			}
			b, err = ioutil.ReadAll(reader)
			if err != nil {
				return fmt.Errorf("error reading from bucket object %q: %s", jwtKey, err)
			}
		} else {
			// Load the key from the filesystem.
			b, err = ioutil.ReadFile(jwtKey)
			if err != nil {
				return fmt.Errorf("error reading JWT key from file %q: %s", jwtKey, err)
			}
		}
		block, _ := pem.Decode(b)
		rsaKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return fmt.Errorf("error parsing key from %q: %s", jwtKey, err)
		}
	}
	s := uploadserver.New(uploadserver.Options{
		AllowCORS:        *allowCORS,
		GradeLocally:     *gradeLocally,
		ServerURL:        serverURL,
		UploadDir:        *uploadDir,
		Channel:          q,
		QueueName:        *autograderQueue,
		UseOpenID:        *useOpenID,
		AllowedUsers:     allowedUsers,
		AuthEndpoint:     endpoint,
		UserinfoEndpoint: userinfoEndpoint,
		// ClientID should be obtained from the Open ID Connect provider.
		ClientID: os.Getenv("CLIENT_ID"),
		// ClientSecret should be obtained from the Open ID Connect provider.
		ClientSecret: os.Getenv("CLIENT_SECRET"),
		// CookieAuthKey should be a random string of 16 characters.
		CookieAuthKey: os.Getenv("COOKIE_AUTH_KEY"),
		// CookieEncryptKey should be a random string of 16 or 32 characters.
		CookieEncryptKey: os.Getenv("COOKIE_ENCRYPT_KEY"),
		// Use secure cookie when using HTTPS.
		SecureCookie: *useHTTPS || *secureCookie,
		// HashSalt should be a random string.
		HashSalt:         os.Getenv("HASH_SALT"),
		StaticDir:        *staticDir,
		HTTPRedirectPort: *httpRedirectPort,
		Autograder:       ag,
		LogToBucket:      *logToBucket,
		LogBucketName:    os.Getenv("LOG_BUCKET"),
		ProjectID:        os.Getenv("GCP_PROJECT"),
		UseJWT:           *useJWT,
		PrivateKey:       rsaKey,
	})
	if *gradeLocally {
		fmt.Printf("\n  Serving on %s (grading locally)\n\n", serverURL)
	} else {
		go s.ListenForReports(ch)
		fmt.Printf("\n  Serving on %s (with grading queue)\n\n", serverURL)
	}
	if *useHTTPS {
		return s.ListenAndServeTLS(addr, *sslCertFile, *sslKeyFile)
	}
	return s.ListenAndServe(addr)
}
