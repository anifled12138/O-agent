package pluginforge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/capsule"
	"axiom.local/agent/internal/pluginmanifest"
)

const capsuleGojaVersion = "v0.0.0-20260911104922-fabc3b8078ad"

// GenerateFromCapsule creates a compiled, process-isolated reference candidate.
// It deliberately preserves the verified JavaScript implementation instead of
// asking a model to translate it. Native rewrites can later be compared against
// this semantic baseline by the evaluation harness.
func (s *Service) GenerateFromCapsule(ctx context.Context, userID, projectID string, source capsule.Manifest) (Project, error) {
	if err := source.Validate(); err != nil {
		return Project{}, err
	}
	if !objectSchema(source.Contract.InputSchema) || !objectSchema(source.Contract.OutputSchema) {
		return Project{}, errors.New("global plugin tools require object input and output schemas")
	}
	unlock := s.lockSourceProject(projectID)
	defer unlock()
	p, err := s.repo.Transition(ctx, userID, projectID, StateGenerating, "")
	if err != nil {
		return Project{}, err
	}
	manifest := capsulePluginManifest(p, source)
	if err = manifest.Validate(); err != nil {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateGenerationFailed, err.Error())
		return failed, err
	}
	if err = writeCapsuleProject(p, manifest, source); err != nil {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateGenerationFailed, err.Error())
		return failed, err
	}
	goExe, err := s.findGo()
	if err == nil {
		var output string
		output, err = runCommand(ctx, filepath.Join(p.SourceDir, "backend"), goExe, "mod", "tidy")
		if err != nil {
			err = fmt.Errorf("resolve capsule runtime dependencies: %s", strings.TrimSpace(output))
		}
	}
	if err != nil {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateGenerationFailed, err.Error())
		return failed, err
	}
	if err = commitGeneratedProject(ctx, p.SourceDir); err != nil {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateGenerationFailed, err.Error())
		return failed, err
	}
	p, err = s.repo.Transition(ctx, userID, projectID, StateGenerated, "")
	if err == nil {
		s.audit(ctx, p, "capsule.reference.generated", map[string]any{"capsuleId": source.ID, "capsuleDigest": source.Digest, "evidenceCases": len(source.Evidence)})
	}
	return p, err
}

func capsulePluginManifest(project Project, source capsule.Manifest) pluginmanifest.Manifest {
	pluginID := "workspace." + project.Slug
	return pluginmanifest.Manifest{
		SpecVersion: pluginmanifest.SpecV2,
		ID:          pluginID,
		Name:        project.Name,
		Version:     "0.2." + time.Now().UTC().Format("20060102150405"),
		Description: project.Description,
		Runtime:     &pluginmanifest.Runtime{Backend: &pluginmanifest.Backend{Artifact: "backend/plugin.exe", Protocol: "axiom.rpc/v2", ShutdownMillis: 10000}},
		Exports: pluginmanifest.Exports{Tools: []pluginmanifest.ToolExport{{
			ID: pluginID + ".invoke", Summary: source.Contract.Summary, Tags: append([]string(nil), source.Contract.Tags...),
			Visibility: "discoverable", Risk: "pure-compute", Executor: pluginmanifest.Executor{Kind: "backend", Target: "capability.invoke"},
			InputSchema: append(json.RawMessage(nil), source.Contract.InputSchema...), OutputSchema: append(json.RawMessage(nil), source.Contract.OutputSchema...),
		}}},
		Upgrade: pluginmanifest.Upgrade{Strategy: "drain", PinActiveCalls: true, StateVersion: 1},
	}
}

func writeCapsuleProject(project Project, manifest pluginmanifest.Manifest, source capsule.Manifest) error {
	backendDir := filepath.Join(project.SourceDir, "backend")
	if err := os.MkdirAll(backendDir, 0o700); err != nil {
		return err
	}
	manifestRaw, _ := json.MarshalIndent(manifest, "", "  ")
	evidenceRaw, _ := json.Marshal(source.Evidence)
	timeout := source.Contract.Limits.Normalized().TimeoutMillis
	maxOutput := source.Contract.Limits.Normalized().MaxOutputKiB * 1024
	backendSource, err := format.Source([]byte(capsuleBackendSource(strconv.Quote(source.Program), timeout, maxOutput)))
	if err != nil {
		return fmt.Errorf("format generated capsule backend: %w", err)
	}
	testSource, err := format.Source([]byte(capsuleBackendTestSource(strconv.Quote(string(evidenceRaw)))))
	if err != nil {
		return fmt.Errorf("format generated capsule replay test: %w", err)
	}
	files := map[string][]byte{
		filepath.Join(project.SourceDir, "plugin.json"): manifestRaw,
		filepath.Join(project.SourceDir, ".gitignore"):  []byte("build/\n"),
		filepath.Join(project.SourceDir, "README.md"):   []byte(fmt.Sprintf("# %s\n\nCompiled reference release for Capsule `%s` at digest `%s`.\n\nThis candidate preserves the verified Capsule behavior and contains %d replay cases. It requests no host permissions.\n", project.Name, source.ID, source.Digest, len(source.Evidence))),
		filepath.Join(backendDir, "go.mod"):             []byte("module axiom.generated/" + project.Slug + "\n\ngo 1.27.0\n\nrequire github.com/dop251/goja " + capsuleGojaVersion + "\n"),
		filepath.Join(backendDir, "main.go"):            backendSource,
		filepath.Join(backendDir, "main_test.go"):       testSource,
	}
	for path, contents := range files {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func objectSchema(raw json.RawMessage) bool {
	var schema struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &schema) == nil && schema.Type == "object"
}

func capsuleBackendSource(program string, timeoutMillis, maxOutputBytes int) string {
	return fmt.Sprintf(`package main

import (
 "bufio"
 "bytes"
 "encoding/json"
 "errors"
 "fmt"
 "os"
 "time"

 "github.com/dop251/goja"
)

const capabilityProgram = %s
const capabilityTimeout = %d * time.Millisecond
const capabilityMaxOutput = %d

type request struct { ID string `+"`json:\"id\"`"+`; Method string `+"`json:\"method\"`"+`; Params json.RawMessage `+"`json:\"params,omitempty\"`"+` }
type response struct { ID string `+"`json:\"id\"`"+`; Result json.RawMessage `+"`json:\"result,omitempty\"`"+`; Error string `+"`json:\"error,omitempty\"`"+` }

func main() {
 scanner:=bufio.NewScanner(os.Stdin);scanner.Buffer(make([]byte,64<<10),1<<20)
 encoder:=json.NewEncoder(os.Stdout)
 for scanner.Scan(){
  var req request
  if err:=json.Unmarshal(scanner.Bytes(),&req);err!=nil{_ = encoder.Encode(response{Error:err.Error()});continue}
  result,err:=handle(req);out:=response{ID:req.ID,Result:result}
  if err!=nil{out.Result=nil;out.Error=err.Error()};_ = encoder.Encode(out)
  if req.Method=="plugin.shutdown"{return}
 }
}

func handle(req request)(json.RawMessage,error){
 switch req.Method{
 case "plugin.hello":return json.RawMessage(`+"`{\"protocol\":\"axiom.rpc/v2\",\"status\":\"ready\"}`"+`),nil
 case "plugin.health":return json.RawMessage(`+"`{\"status\":\"healthy\",\"runtime\":\"capsule-reference\"}`"+`),nil
 case "capabilities.list":return json.RawMessage(`+"`[\"capability.invoke\"]`"+`),nil
 case "plugin.shutdown":return json.RawMessage(`+"`{\"status\":\"stopping\"}`"+`),nil
 case "capability.invoke":
  var input map[string]any
  if err:=json.Unmarshal(req.Params,&input);err!=nil{return nil,err}
  delete(input,"_axiomCapabilityId")
  raw,err:=json.Marshal(input);if err!=nil{return nil,err};return execute(raw)
 default:return nil,fmt.Errorf("unknown method %%s",req.Method)
 }
}

func execute(raw json.RawMessage)(json.RawMessage,error){
 decoder:=json.NewDecoder(bytes.NewReader(raw));decoder.UseNumber();var input any
 if err:=decoder.Decode(&input);err!=nil{return nil,err}
 vm:=goja.New();if err:=vm.Set("input",input);err!=nil{return nil,err}
 timer:=time.AfterFunc(capabilityTimeout,func(){vm.Interrupt("execution deadline exceeded")});defer timer.Stop()
 value,err:=vm.RunString("(function(input){\n"+capabilityProgram+"\n})(input)")
 if err!=nil{return nil,err};if goja.IsUndefined(value){return nil,errors.New("capability returned undefined")}
 output,err:=json.Marshal(value.Export());if err!=nil{return nil,err}
 if len(output)>capabilityMaxOutput{return nil,errors.New("capability output limit exceeded")}
 return output,nil
}
`, program, timeoutMillis, maxOutputBytes)
}

func capsuleBackendTestSource(evidence string) string {
	return fmt.Sprintf(`package main

import (
 "encoding/json"
 "reflect"
 "testing"
)

const evidenceJSON = %s

func TestCapsuleEvidenceReplay(t *testing.T){
 var cases []struct{ID string `+"`json:\"id\"`"+`;Input json.RawMessage `+"`json:\"input\"`"+`;Output json.RawMessage `+"`json:\"output\"`"+`}
 if err:=json.Unmarshal([]byte(evidenceJSON),&cases);err!=nil{t.Fatal(err)}
 if len(cases)==0{t.Fatal("capsule has no evidence")}
 for _,item:=range cases{t.Run(item.ID,func(t *testing.T){
  actual,err:=execute(item.Input);if err!=nil{t.Fatal(err)}
  var got,want any;if json.Unmarshal(actual,&got)!=nil||json.Unmarshal(item.Output,&want)!=nil{t.Fatal("invalid replay JSON")}
  if !reflect.DeepEqual(got,want){t.Fatalf("replay changed: got %%s want %%s",actual,item.Output)}
 })}
}
`, evidence)
}
