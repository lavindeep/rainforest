package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const graphPlanFixture = `{
  "prior_state": {"values": {"root_module": {"resources": [
    {"address":"aws_vpc.main","mode":"managed","type":"aws_vpc","name":"main","values":{"id":"vpc-main"}},
    {"address":"aws_subnet.old","mode":"managed","type":"aws_subnet","name":"old","depends_on":["aws_vpc.main"],"values":{"id":"subnet-old","vpc_id":"vpc-main"}},
    {"address":"aws_instance.web[0]","mode":"managed","type":"aws_instance","name":"web","depends_on":["aws_subnet.old"],"values":{"subnet_id":"subnet-old"}},
    {"address":"data.aws_region.current","mode":"data","type":"aws_region","name":"current"}
  ]}}},
  "planned_values": {"root_module": {"resources": [
    {"address":"aws_vpc.main","mode":"managed","type":"aws_vpc","name":"main"},
    {"address":"aws_subnet.new","mode":"managed","type":"aws_subnet","name":"new"},
    {"address":"aws_instance.web[0]","mode":"managed","type":"aws_instance","name":"web"},
    {"address":"aws_security_group.app","mode":"managed","type":"aws_security_group","name":"app"},
    {"address":"data.aws_region.current","mode":"data","type":"aws_region","name":"current"}
  ]}},
  "configuration": {"root_module": {
    "resources": [
      {"address":"aws_vpc.main","mode":"managed"},
      {"address":"aws_subnet.new","mode":"managed","expressions":{"vpc_id":{"references":["aws_vpc.main.id","var.secret"]}}},
      {"address":"aws_instance.web","mode":"managed","depends_on":["aws_subnet.new"],"expressions":{"ami":{"references":["data.aws_ami.secret.id"]},"subnet_id":{"references":["aws_subnet.new.id"]}}},
      {"address":"aws_security_group.app","mode":"managed","expressions":{"vpc_id":{"references":["aws_vpc.main.id","aws_vpc.main.id"]}}}
    ]
  }},
  "resource_changes": [
    {"address":"aws_vpc.main","mode":"managed","type":"aws_vpc","name":"main","change":{"actions":["no-op"],"after":{"token":"CANARY_DO_NOT_LEAK"}}},
    {"address":"aws_subnet.old","mode":"managed","type":"aws_subnet","name":"old","change":{"actions":["delete"]}},
    {"address":"aws_subnet.new","mode":"managed","type":"aws_subnet","name":"new","change":{"actions":["create"]}},
    {"address":"aws_instance.web[0]","mode":"managed","type":"aws_instance","name":"web","change":{"actions":["update"]}},
    {"address":"aws_security_group.app","mode":"managed","type":"aws_security_group","name":"app","change":{"actions":["delete","create"]}},
    {"address":"data.aws_region.current","mode":"data","type":"aws_region","name":"current","change":{"actions":["read"]}}
  ]
}`

type graphBody struct {
	View  string      `json:"view"`
	RunID string      `json:"runId"`
	Nodes []graphNode `json:"nodes"`
	Edges []graphEdge `json:"edges"`
}

func graphByAddress(nodes []graphNode, address string) (graphNode, bool) {
	for _, node := range nodes {
		if node.Address == address {
			return node, true
		}
	}
	return graphNode{}, false
}

func TestGraphPlanViewsAreSanitizedStableAndClassified(t *testing.T) {
	workspace := initializedWorkspace(t)
	planScript(t, "exit 0", "printf '%s\\n' '"+graphPlanFixture+"'")
	s := newPlanTestServerIn(t, workspace)
	started := startPlan(t, s)
	_, summary := waitForSummary(t, s)
	if summary.State != "succeeded" {
		t.Fatalf("plan state = %q, want succeeded", summary.State)
	}

	for _, view := range []string{"proposed", "diff"} {
		rec := authenticatedRequest(t, s, http.MethodGet, "/api/graph?view="+view, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (body %q)", view, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "CANARY_DO_NOT_LEAK") || strings.Contains(rec.Body.String(), "data.aws_region") {
			t.Fatalf("%s graph leaked values or data resources: %s", view, rec.Body.String())
		}
		var body graphBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", view, err)
		}
		if body.View != view || body.RunID != started.RunID {
			t.Errorf("%s identity = %q/%q, want %q/%q", view, body.View, body.RunID, view, started.RunID)
		}
		for i := 1; i < len(body.Nodes); i++ {
			if body.Nodes[i-1].Address > body.Nodes[i].Address {
				t.Errorf("%s nodes are not sorted: %+v", view, body.Nodes)
			}
		}
		for i := 1; i < len(body.Edges); i++ {
			if body.Edges[i-1].ID > body.Edges[i].ID {
				t.Errorf("%s edges are not sorted: %+v", view, body.Edges)
			}
		}
	}

	rec := authenticatedRequest(t, s, http.MethodGet, "/api/graph?view=diff", nil)
	var diff graphBody
	decode(t, rec, &diff)
	wantStates := map[string]string{
		"aws_vpc.main":           "unchanged",
		"aws_subnet.old":         "destroyed",
		"aws_subnet.new":         "created",
		"aws_instance.web[0]":    "changed",
		"aws_security_group.app": "replaced",
	}
	for address, state := range wantStates {
		node, ok := graphByAddress(diff.Nodes, address)
		if !ok || node.State != state {
			t.Errorf("diff node %s = %+v, want state %q", address, node, state)
		}
	}
	if node, _ := graphByAddress(diff.Nodes, "aws_subnet.new"); node.Parent != "aws_vpc.main" || node.Kind != "subnet" {
		t.Errorf("new subnet = %+v, want vpc parent and subnet kind", node)
	}
	if node, _ := graphByAddress(diff.Nodes, "aws_instance.web[0]"); node.Parent != "aws_subnet.new" || node.Kind != "instance" {
		t.Errorf("instance = %+v, want new subnet parent and instance kind", node)
	}
	wantEdges := map[string]string{
		"dependency:aws_vpc.main->aws_subnet.old":         "closed",
		"dependency:aws_subnet.old->aws_instance.web[0]":  "closed",
		"dependency:aws_vpc.main->aws_subnet.new":         "opened",
		"dependency:aws_subnet.new->aws_instance.web[0]":  "opened",
		"dependency:aws_vpc.main->aws_security_group.app": "opened",
	}
	for _, edge := range diff.Edges {
		if state, ok := wantEdges[edge.ID]; ok {
			if edge.State != state || edge.Kind != "dependency" {
				t.Errorf("edge %s = %+v, want %s dependency", edge.ID, edge, state)
			}
			delete(wantEdges, edge.ID)
		}
	}
	if len(wantEdges) != 0 {
		t.Errorf("missing diff edges: %v", wantEdges)
	}
}

func TestGraphCurrentDefaultsViewAndUsesFreshState(t *testing.T) {
	workspace := initializedWorkspace(t)
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("GRAPH_ARGS_FILE", argsFile)
	stubTerraform(t, `printf '%s\n' "$*" > "$GRAPH_ARGS_FILE"
printf '%s\n' '{"values":{"root_module":{"resources":[
  {"address":"aws_vpc.main","mode":"managed","type":"aws_vpc","name":"main"},
  {"address":"aws_subnet.public","mode":"managed","type":"aws_subnet","name":"public","depends_on":["aws_vpc.main"]},
  {"address":"data.aws_region.current","mode":"data","type":"aws_region","name":"current"}
]}}}'`)

	rec := get(t, newPlanTestServerIn(t, workspace), "/api/graph")
	var body graphBody
	decode(t, rec, &body)
	if body.View != "current" || body.RunID != "" || len(body.Nodes) != 2 || len(body.Edges) != 1 {
		t.Fatalf("current graph = %+v, want two managed nodes and one edge", body)
	}
	if body.Edges[0].Source != "aws_vpc.main" || body.Edges[0].Target != "aws_subnet.public" {
		t.Errorf("edge = %+v, want dependency to dependent", body.Edges[0])
	}
	if got, err := os.ReadFile(argsFile); err != nil || string(got) != "show -json\n" {
		t.Errorf("terraform argv = %q, %v; want show -json", got, err)
	}
}

func TestGraphStatuses(t *testing.T) {
	t.Run("invalid view", func(t *testing.T) {
		rec := get(t, newPlanTestServerIn(t, t.TempDir()), "/api/graph?view=future")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
	t.Run("current terraform missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		rec := get(t, newPlanTestServerIn(t, initializedWorkspace(t)), "/api/graph")
		if rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", rec.Code)
		}
	})
	t.Run("current uninitialized", func(t *testing.T) {
		stubTerraform(t, "exit 0")
		rec := get(t, newPlanTestServerIn(t, t.TempDir()), "/api/graph")
		if rec.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", rec.Code)
		}
	})
	t.Run("current no state", func(t *testing.T) {
		workspace := initializedWorkspace(t)
		stubTerraform(t, `echo 'No state file was found!' >&2; exit 1`)
		rec := get(t, newPlanTestServerIn(t, workspace), "/api/graph")
		var body graphBody
		decode(t, rec, &body)
		if len(body.Nodes) != 0 || len(body.Edges) != 0 {
			t.Errorf("body = %+v, want empty graph", body)
		}
	})
	t.Run("current invalid json", func(t *testing.T) {
		workspace := initializedWorkspace(t)
		stubTerraform(t, `echo 'not json'`)
		rec := get(t, newPlanTestServerIn(t, workspace), "/api/graph")
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("proposed no run", func(t *testing.T) {
		rec := get(t, newPlanTestServerIn(t, t.TempDir()), "/api/graph?view=proposed")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

func TestParsePlanGraphsKeepsChildModuleInstancesAligned(t *testing.T) {
	raw := []byte(`{
  "planned_values":{"root_module":{"child_modules":[
    {"resources":[
      {"address":"module.net[0].aws_vpc.main","mode":"managed","type":"aws_vpc","name":"main"},
      {"address":"module.net[0].aws_subnet.public","mode":"managed","type":"aws_subnet","name":"public"}
    ]},
    {"resources":[
      {"address":"module.net[1].aws_vpc.main","mode":"managed","type":"aws_vpc","name":"main"},
      {"address":"module.net[1].aws_subnet.public","mode":"managed","type":"aws_subnet","name":"public"}
    ]}
  ]}},
  "configuration":{"root_module":{"module_calls":{"net":{"module":{"resources":[
    {"address":"aws_vpc.main","mode":"managed"},
    {"address":"aws_subnet.public","mode":"managed","expressions":{"vpc_id":{"references":["aws_vpc.main.id"]}}}
  ]}}}}}
}`)
	graphs, err := parsePlanGraphs(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{
		"dependency:module.net[0].aws_vpc.main->module.net[0].aws_subnet.public",
		"dependency:module.net[1].aws_vpc.main->module.net[1].aws_subnet.public",
	}
	if len(graphs.proposed.Edges) != len(want) {
		t.Fatalf("edges = %+v, want %v", graphs.proposed.Edges, want)
	}
	for i, edge := range graphs.proposed.Edges {
		if edge.ID != want[i] {
			t.Errorf("edge %d = %q, want %q", i, edge.ID, want[i])
		}
	}
}

func TestParsePlanGraphsPropagatesOnlyReferencedModuleInputs(t *testing.T) {
	raw := []byte(`{
  "planned_values":{"root_module":{
    "resources":[
      {"address":"terraform_data.keep","mode":"managed","type":"terraform_data","name":"keep"},
      {"address":"terraform_data.many[0]","mode":"managed","type":"terraform_data","name":"many"},
      {"address":"terraform_data.many[1]","mode":"managed","type":"terraform_data","name":"many"}
    ],
    "child_modules":[
      {"resources":[
        {"address":"module.outer[0].terraform_data.used","mode":"managed","type":"terraform_data","name":"used"},
        {"address":"module.outer[0].terraform_data.unused","mode":"managed","type":"terraform_data","name":"unused"}
      ],"child_modules":[{"resources":[
        {"address":"module.outer[0].module.inner.terraform_data.deep","mode":"managed","type":"terraform_data","name":"deep"}
      ]}]},
      {"resources":[
        {"address":"module.outer[1].terraform_data.used","mode":"managed","type":"terraform_data","name":"used"},
        {"address":"module.outer[1].terraform_data.unused","mode":"managed","type":"terraform_data","name":"unused"}
      ],"child_modules":[{"resources":[
        {"address":"module.outer[1].module.inner.terraform_data.deep","mode":"managed","type":"terraform_data","name":"deep"}
      ]}]}
    ]
  }},
  "configuration":{"root_module":{
    "resources":[
      {"address":"terraform_data.keep","mode":"managed"},
      {"address":"terraform_data.many","mode":"managed"}
    ],
    "module_calls":{"outer":{
      "expressions":{"seed":{"references":["terraform_data.keep","terraform_data.many"]}},
      "module":{
        "resources":[
          {"address":"terraform_data.used","mode":"managed","expressions":{"input":{"references":["var.seed"]}}},
          {"address":"terraform_data.unused","mode":"managed","expressions":{"input":{"references":["var.other"]}}}
        ],
        "module_calls":{"inner":{
          "expressions":{"forwarded":{"references":["var.seed"]}},
          "module":{"resources":[
            {"address":"terraform_data.deep","mode":"managed","expressions":{"input":{"references":["var.forwarded"]}}}
          ]}
        }}
      }
    }}
  }}
}`)
	graphs, err := parsePlanGraphs(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{
		"dependency:terraform_data.keep->module.outer[0].module.inner.terraform_data.deep",
		"dependency:terraform_data.keep->module.outer[0].terraform_data.used",
		"dependency:terraform_data.keep->module.outer[1].module.inner.terraform_data.deep",
		"dependency:terraform_data.keep->module.outer[1].terraform_data.used",
	}
	if len(graphs.proposed.Edges) != len(want) {
		t.Fatalf("edges = %+v, want %v", graphs.proposed.Edges, want)
	}
	for i, edge := range graphs.proposed.Edges {
		if edge.ID != want[i] {
			t.Errorf("edge %d = %q, want %q", i, edge.ID, want[i])
		}
	}
}

func TestParsePlanGraphsCollectsNestedExpressionReferences(t *testing.T) {
	raw := []byte(`{
  "planned_values":{"root_module":{"resources":[
    {"address":"terraform_data.keep","mode":"managed","type":"terraform_data","name":"keep"},
    {"address":"terraform_data.use","mode":"managed","type":"terraform_data","name":"use"}
  ]}},
  "configuration":{"root_module":{"resources":[
    {"address":"terraform_data.keep","mode":"managed"},
    {"address":"terraform_data.use","mode":"managed","expressions":{"input":[
      {"nested":{"references":["terraform_data.keep.output"]}},
      {"constant_value":{"secret":"CANARY_NESTED_CONSTANT"}}
    ]}}
  ]}}
}`)
	graphs, err := parsePlanGraphs(raw)
	if err != nil {
		t.Fatalf("parse nested expressions: %v", err)
	}
	if len(graphs.proposed.Edges) != 1 || graphs.proposed.Edges[0].ID != "dependency:terraform_data.keep->terraform_data.use" {
		t.Fatalf("edges = %+v, want nested reference edge", graphs.proposed.Edges)
	}
	encoded, err := json.Marshal(graphs.proposed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "CANARY_NESTED_CONSTANT") {
		t.Fatalf("graph leaked constant value: %s", encoded)
	}
}

func TestParsePlanGraphsUsesOnlySemanticContainmentReferences(t *testing.T) {
	raw := []byte(`{
  "planned_values":{"root_module":{
    "resources":[
      {"address":"aws_vpc.parent","mode":"managed","type":"aws_vpc","name":"parent"},
      {"address":"aws_vpc.unrelated","mode":"managed","type":"aws_vpc","name":"unrelated"},
      {"address":"aws_subnet.ambiguous","mode":"managed","type":"aws_subnet","name":"ambiguous"}
    ],
	    "child_modules":[{"resources":[
	      {"address":"module.network.aws_vpc.unrelated","mode":"managed","type":"aws_vpc","name":"unrelated"},
	      {"address":"module.network.aws_subnet.child","mode":"managed","type":"aws_subnet","name":"child"}
    ]}]
  }},
  "configuration":{"root_module":{
    "resources":[
      {"address":"aws_vpc.parent","mode":"managed"},
      {"address":"aws_vpc.unrelated","mode":"managed"},
      {"address":"aws_subnet.ambiguous","mode":"managed","expressions":{"vpc_id":{"references":["aws_vpc.parent.id","aws_vpc.unrelated.id"]}}}
    ],
    "module_calls":{"network":{
	      "expressions":{"vpc":{"references":["aws_vpc.parent.id"]}},
	      "module":{"resources":[
	        {"address":"aws_vpc.unrelated","mode":"managed"},
	        {"address":"aws_subnet.child","mode":"managed","depends_on":["aws_vpc.unrelated"],"expressions":{"vpc_id":{"references":["var.vpc"]}}}
      ]}
    }}
  }}
}`)
	graphs, err := parsePlanGraphs(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	child, _ := graphByAddress(graphs.proposed.Nodes, "module.network.aws_subnet.child")
	if child.Parent != "aws_vpc.parent" {
		t.Errorf("child parent = %q, want aws_vpc.parent", child.Parent)
	}
	ambiguous, _ := graphByAddress(graphs.proposed.Nodes, "aws_subnet.ambiguous")
	if ambiguous.Parent != "" {
		t.Errorf("ambiguous parent = %q, want empty", ambiguous.Parent)
	}
	wantEdges := map[string]bool{
		"dependency:aws_vpc.parent->module.network.aws_subnet.child":                   true,
		"dependency:module.network.aws_vpc.unrelated->module.network.aws_subnet.child": true,
	}
	for _, edge := range graphs.proposed.Edges {
		delete(wantEdges, edge.ID)
	}
	if len(wantEdges) != 0 {
		t.Errorf("missing generic edges: %v", wantEdges)
	}
}

func TestParseCurrentGraphUsesOnlyExactContainmentIDs(t *testing.T) {
	raw := []byte(`{"values":{"root_module":{"resources":[
    {"address":"aws_vpc.main","mode":"managed","type":"aws_vpc","name":"main","values":{"id":"vpc-1","secret":"CANARY_STATE_VALUE"}},
    {"address":"aws_vpc.other","mode":"managed","type":"aws_vpc","name":"other","values":{"id":"vpc-2"}},
    {"address":"aws_subnet.public","mode":"managed","type":"aws_subnet","name":"public","depends_on":["aws_vpc.other"],"values":{"id":"subnet-1","vpc_id":"vpc-1"}},
    {"address":"aws_instance.web","mode":"managed","type":"aws_instance","name":"web","values":{"subnet_id":"subnet-1"}}
  ]}}}`)
	graph, err := parseCurrentGraph(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	subnet, _ := graphByAddress(graph.Nodes, "aws_subnet.public")
	if subnet.Parent != "aws_vpc.main" {
		t.Errorf("subnet parent = %q, want exact ID match aws_vpc.main", subnet.Parent)
	}
	instance, _ := graphByAddress(graph.Nodes, "aws_instance.web")
	if instance.Parent != "aws_subnet.public" {
		t.Errorf("instance parent = %q, want exact ID match aws_subnet.public", instance.Parent)
	}
	encoded, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "values") || strings.Contains(string(encoded), "CANARY_STATE_VALUE") {
		t.Fatalf("graph leaked state values: %s", encoded)
	}
}

func TestResolveDependencyOmitsAmbiguousRootInstances(t *testing.T) {
	addresses := []string{
		"terraform_data.source[0]",
		"terraform_data.source[1]",
		"terraform_data.target[0]",
	}
	if got := resolveDependency("terraform_data.source", "terraform_data.target[0]", addresses); len(got) != 0 {
		t.Errorf("resolved = %v, want no ambiguous root expansion", got)
	}
}

func TestParsePlanGraphsOmitsSingleIncompatibleModuleInstance(t *testing.T) {
	raw := []byte(`{
  "planned_values":{"root_module":{"child_modules":[
    {"resources":[
      {"address":"module.net[0].aws_vpc.source","mode":"managed","type":"aws_vpc","name":"source"}
    ]},
    {"resources":[
      {"address":"module.net[1].aws_subnet.target","mode":"managed","type":"aws_subnet","name":"target"}
    ]}
  ]}},
  "configuration":{"root_module":{"module_calls":{"net":{"module":{"resources":[
    {"address":"aws_vpc.source","mode":"managed"},
    {"address":"aws_subnet.target","mode":"managed","expressions":{"vpc_id":{"references":["aws_vpc.source.id"]}}}
  ]}}}}}
}`)
	graphs, err := parsePlanGraphs(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(graphs.proposed.Edges) != 0 {
		t.Errorf("edges = %+v, want none across sibling module instances", graphs.proposed.Edges)
	}
	target, _ := graphByAddress(graphs.proposed.Nodes, "module.net[1].aws_subnet.target")
	if target.Parent != "" {
		t.Errorf("target parent = %q, want empty across sibling module instances", target.Parent)
	}
}

func TestParsePlanGraphsAlignsAncestorModuleInstances(t *testing.T) {
	raw := []byte(`{
  "planned_values":{"root_module":{"child_modules":[
    {"resources":[{"address":"module.outer[\"west.one\"].terraform_data.source","mode":"managed","type":"terraform_data","name":"source"}],"child_modules":[
      {"resources":[{"address":"module.outer[\"west.one\"].module.inner[0].terraform_data.target","mode":"managed","type":"terraform_data","name":"target"}]}
    ]},
    {"resources":[{"address":"module.outer[\"east\"].terraform_data.source","mode":"managed","type":"terraform_data","name":"source"}],"child_modules":[
      {"resources":[{"address":"module.outer[\"east\"].module.inner[0].terraform_data.target","mode":"managed","type":"terraform_data","name":"target"}]}
    ]}
  ]}},
  "configuration":{"root_module":{"module_calls":{"outer":{"module":{
    "resources":[{"address":"terraform_data.source","mode":"managed"}],
    "module_calls":{"inner":{
      "expressions":{"seed":{"references":["terraform_data.source.output"]}},
      "module":{"resources":[
        {"address":"terraform_data.target","mode":"managed","expressions":{"input":{"references":["var.seed"]}}}
      ]}
    }}
  }}}}}
}`)
	graphs, err := parsePlanGraphs(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{
		"dependency:module.outer[\"east\"].terraform_data.source->module.outer[\"east\"].module.inner[0].terraform_data.target",
		"dependency:module.outer[\"west.one\"].terraform_data.source->module.outer[\"west.one\"].module.inner[0].terraform_data.target",
	}
	if len(graphs.proposed.Edges) != len(want) {
		t.Fatalf("edges = %+v, want %v", graphs.proposed.Edges, want)
	}
	for i, edge := range graphs.proposed.Edges {
		if edge.ID != want[i] {
			t.Errorf("edge %d = %q, want %q", i, edge.ID, want[i])
		}
	}
}

func TestCloseReturnsWhenDetachedChildHoldsCurrentGraphPipes(t *testing.T) {
	oldCloseGrace := closeGrace
	closeGrace = 100 * time.Millisecond
	t.Cleanup(func() { closeGrace = oldCloseGrace })

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	workspace := initializedWorkspace(t)
	holderPIDFile := filepath.Join(workspace, "graph-holder-pid")
	t.Setenv("GO_WANT_PLAN_PIPE_HOLDER", "1")
	t.Setenv("PLAN_PIPE_HOLDER_PID_FILE", holderPIDFile)
	t.Setenv("GRAPH_PIPE_HOLDER_BINARY", testBinary)
	stubTerraform(t, `trap '' INT
"$GRAPH_PIPE_HOLDER_BINARY" -test.run '^TestPlanPipeHolderProcess$' &
: > "$PWD/graph-ready"
while :; do /bin/sleep 1; done`)
	s := newPlanTestServerIn(t, workspace)
	requestDone := make(chan struct{})
	go func() {
		authenticatedRequest(t, s, http.MethodGet, "/api/graph", nil)
		close(requestDone)
	}()
	waitForFile(t, filepath.Join(workspace, "graph-ready"))
	waitForFile(t, holderPIDFile)
	rawHolderPID, err := os.ReadFile(holderPIDFile)
	if err != nil {
		t.Fatalf("read holder PID: %v", err)
	}
	holderPID, err := strconv.Atoi(strings.TrimSpace(string(rawHolderPID)))
	if err != nil {
		t.Fatalf("parse holder PID %q: %v", rawHolderPID, err)
	}
	holderCleaned := false
	cleanupHolder := func() {
		if err := syscall.Kill(holderPID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill graph pipe holder %d: %v", holderPID, err)
		}
		waitForProcessExit(t, holderPID)
		holderCleaned = true
	}
	t.Cleanup(func() {
		if !holderCleaned {
			cleanupHolder()
		}
	})

	closeDone := make(chan error, 1)
	startedAt := time.Now()
	go func() { closeDone <- s.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(closeGrace + time.Second):
		cleanupHolder()
		<-closeDone
		t.Fatal("Close did not return after the bounded current graph wait")
	}
	if elapsed := time.Since(startedAt); elapsed < closeGrace || elapsed > closeGrace+time.Second {
		t.Errorf("Close elapsed = %s, want between %s and %s", elapsed, closeGrace, closeGrace+time.Second)
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("current graph request did not return after Close")
	}
	if err := syscall.Kill(holderPID, 0); err != nil {
		t.Errorf("detached graph pipe holder is not alive after Close: %v", err)
	}
	cleanupHolder()
}
