package uisrv

import (
	"context"
	"fmt"
	"gopkg.in/yaml.v3"
	"log"
	"net/http"
	"path"
	"strings"
	"text/template"
	"time"

	"github.com/jaronoff97/opamp-operator-server/data"

	"github.com/open-telemetry/opamp-go/protobufs"
)

var htmlDir string
var srv *http.Server

var logger = log.New(log.Default().Writer(), "[UI] ", log.Default().Flags()|log.Lmsgprefix|log.Lmicroseconds)

var (
	funcs = template.FuncMap{
		"getNameFromKey":          getNameFromKey,
		"getNamespaceFromKey":     getNamespaceFromKey,
		"getKeyFromNameNamespace": getKeyFromNameNamespace,
		"getSpec":                 getSpec,
	}
)

func Start(rootDir string) {
	htmlDir = path.Join(rootDir, "uisrv/html")

	mux := http.NewServeMux()
	mux.HandleFunc("/", renderRoot)
	mux.HandleFunc("/agent", renderAgent)
	mux.HandleFunc("/save_config", saveCustomConfigForInstance)
	srv = &http.Server{
		Addr:    "0.0.0.0:4321",
		Handler: mux,
	}
	go srv.ListenAndServe()
}

func Shutdown() {
	srv.Shutdown(context.Background())
}

func renderTemplate(w http.ResponseWriter, htmlTemplateFile string, data interface{}) {

	t, err := template.New("t").Funcs(funcs).ParseFiles(
		path.Join(htmlDir, "header.html"),
		path.Join(htmlDir, htmlTemplateFile),
	)

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		logger.Printf("Error parsing html template %s: %v", htmlTemplateFile, err)
		return
	}

	err = t.Lookup(htmlTemplateFile).Execute(w, data)
	if err != nil {
		// It is too late to send an HTTP status code since content is already written.
		// We can just log the error.
		logger.Printf("Error writing html content %s: %v", htmlTemplateFile, err)
		return
	}
}

// hack to only get the spec
func getSpec(conf protobufs.AgentConfigFile) string {
	m := make(map[string]interface{})
	err := yaml.Unmarshal([]byte(conf.GetBody()), &m)
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	bytes, err := yaml.Marshal(m["spec"])
	if err != nil {
		return ""
	}
	return string(bytes)
}

func getNameAndNamespace(key string) (string, string) {
	s := strings.Split(key, "/")
	// We expect map keys to be of the form name/namespace
	if len(s) != 2 {
		return "", ""
	}
	return s[0], s[1]
}

func getKeyFromNameNamespace(name string, namespace string) string {
	return fmt.Sprintf("%s/%s", name, namespace)
}

// Go templating doesn't allow for multiple returns
func getNameFromKey(key string) string {
	name, _ := getNameAndNamespace(key)
	return name
}

func getNamespaceFromKey(key string) string {
	_, namespace := getNameAndNamespace(key)
	return namespace
}

func renderRoot(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "root.html", data.AllAgents.GetAllAgentsReadonlyClone())
}

func renderAgent(w http.ResponseWriter, r *http.Request) {
	agent := data.AllAgents.GetAgentReadonlyClone(data.InstanceId(r.URL.Query().Get("instanceid")))
	if agent == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	renderMap := map[string]interface{}{
		"Agent":     agent,
		"Name":      r.URL.Query().Get("name"),
		"Namespace": r.URL.Query().Get("namespace"),
	}
	renderTemplate(w, "agent.html", renderMap)
}

func saveCustomConfigForInstance(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	instanceId := data.InstanceId(r.Form.Get("instanceid"))
	agent := data.AllAgents.GetAgentReadonlyClone(instanceId)
	if agent == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	name := r.Form.Get("name")
	namespace := r.Form.Get("namespace")
	if len(name) == 0 || len(namespace) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	configStr := r.PostForm.Get("config")
	config := &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigFile{
			getKeyFromNameNamespace(name, namespace): {Body: []byte(configStr)},
		},
	}

	notifyNextStatusUpdate := make(chan struct{}, 1)
	data.AllAgents.SetCustomConfigForAgent(instanceId, config, notifyNextStatusUpdate)

	// Wait for up to 5 seconds for a Status update, which is expected
	// to be reported by the Agent after we set the remote config.
	timer := time.NewTicker(time.Second * 5)

	select {
	case <-notifyNextStatusUpdate:
	case <-timer.C:
	}

	http.Redirect(w, r, fmt.Sprintf("/agent?instanceid=%s&name=%s&namespace=%s", string(instanceId), name, namespace), http.StatusSeeOther)
}
