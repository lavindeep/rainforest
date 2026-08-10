package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type graphNode struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Parent  string `json:"parent,omitempty"`
	State   string `json:"state,omitempty"`
}

type graphEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
	State  string `json:"state,omitempty"`
}

type graphResponse struct {
	View  string      `json:"view"`
	RunID string      `json:"runId"`
	Nodes []graphNode `json:"nodes"`
	Edges []graphEdge `json:"edges"`
}

type planGraphs struct {
	proposed graphResponse
	diff     graphResponse
}

type graphDocument struct {
	Values        *graphValues        `json:"values"`
	PriorState    *graphState         `json:"prior_state"`
	PlannedValues *graphValues        `json:"planned_values"`
	Configuration *graphConfiguration `json:"configuration"`
	Changes       []graphChange       `json:"resource_changes"`
}

type graphState struct {
	Values *graphValues `json:"values"`
}

type graphValues struct {
	RootModule *graphModule `json:"root_module"`
}

type graphModule struct {
	Resources    []graphResource `json:"resources"`
	ChildModules []graphModule   `json:"child_modules"`
}

type graphResource struct {
	Address   string   `json:"address"`
	Mode      string   `json:"mode"`
	Type      string   `json:"type"`
	Name      string   `json:"name"`
	DependsOn []string `json:"depends_on"`
	Values    struct {
		ID       string `json:"id"`
		VPCID    string `json:"vpc_id"`
		SubnetID string `json:"subnet_id"`
	} `json:"values"`
}

type graphConfiguration struct {
	RootModule *graphConfigModule `json:"root_module"`
}

type graphConfigModule struct {
	Resources  []graphConfigResource      `json:"resources"`
	ModuleCall map[string]graphConfigCall `json:"module_calls"`
}

type graphConfigCall struct {
	Expressions map[string]graphExpression `json:"expressions"`
	Module      *graphConfigModule         `json:"module"`
}

type graphConfigResource struct {
	Address     string                     `json:"address"`
	Mode        string                     `json:"mode"`
	DependsOn   []string                   `json:"depends_on"`
	Expressions map[string]graphExpression `json:"expressions"`
}

type graphExpression struct {
	References []string
	Semantic   map[string][]string
}

func (expression *graphExpression) UnmarshalJSON(raw []byte) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	expression.Semantic = make(map[string][]string)
	collectExpressionReferences(value, "", expression)
	return nil
}

func collectExpressionReferences(value any, semantic string, expression *graphExpression) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			collectExpressionReferences(item, semantic, expression)
		}
	case map[string]any:
		for key, item := range value {
			if key == "constant_value" {
				continue
			}
			if key == "vpc_id" || key == "subnet_id" {
				collectExpressionReferences(item, key, expression)
				continue
			}
			if key != "references" {
				collectExpressionReferences(item, semantic, expression)
				continue
			}
			for _, reference := range stringValues(item) {
				expression.References = append(expression.References, reference)
				if semantic != "" {
					expression.Semantic[semantic] = append(expression.Semantic[semantic], reference)
				}
			}
		}
	}
}

func stringValues(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

type graphChange struct {
	Address string `json:"address"`
	Mode    string `json:"mode"`
	Change  struct {
		Actions []string `json:"actions"`
	} `json:"change"`
}

func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	if view == "" {
		view = "current"
	}
	if view != "current" && view != "proposed" && view != "diff" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "view must be current, proposed, or diff"})
		return
	}
	if view == "current" {
		s.currentGraph(w)
		return
	}
	s.planGraph(w, view)
}

func (s *Server) planGraph(w http.ResponseWriter, view string) {
	s.planMu.Lock()
	if s.plan == nil {
		s.planMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no plan has been run"})
		return
	}
	run := s.plan
	if run.state != "succeeded" || run.graphs == nil {
		response := map[string]string{"error": "plan graph unavailable", "runId": run.id, "state": run.state}
		s.planMu.Unlock()
		writeJSON(w, http.StatusConflict, response)
		return
	}
	var response graphResponse
	if view == "proposed" {
		response = cloneGraph(run.graphs.proposed)
	} else {
		response = cloneGraph(run.graphs.diff)
	}
	response.View = view
	response.RunID = run.id
	s.planMu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) currentGraph(w http.ResponseWriter) {
	s.planMu.Lock()
	if s.closed {
		s.planMu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server is closed"})
		return
	}
	s.graphRuns.Add(1)
	s.planMu.Unlock()
	defer s.graphRuns.Done()

	s.terraformMu.Lock()
	defer s.terraformMu.Unlock()

	s.planMu.Lock()
	busy := s.planStarting || s.plan != nil && s.plan.state == "running"
	closed := s.closed
	s.planMu.Unlock()
	if closed {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server is closed"})
		return
	}
	if busy {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "terraform is busy"})
		return
	}
	terraformPath, err := exec.LookPath("terraform")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "terraform binary not found"})
		return
	}
	if info, err := os.Stat(filepath.Join(s.workspace, ".terraform")); err != nil || !info.IsDir() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "workspace not initialized — run terraform init first"})
		return
	}

	cmd := exec.Command(terraformPath, "show", "-json")
	cmd.Dir = s.workspace
	cmd.Env = terraformEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = closeGrace
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	s.planMu.Lock()
	if s.closed {
		s.planMu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server is closed"})
		return
	}
	if err := cmd.Start(); err != nil {
		s.planMu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "terraform show start: " + err.Error()})
		return
	}
	s.graphPID = cmd.Process.Pid
	s.planMu.Unlock()

	waitErr := cmd.Wait()
	s.planMu.Lock()
	if s.graphPID == cmd.Process.Pid {
		s.graphPID = 0
	}
	s.planMu.Unlock()
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if strings.Contains(strings.ToLower(message), "no state file was found") {
			writeJSON(w, http.StatusOK, emptyGraph("current"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "terraform show failed"})
		return
	}
	graph, err := parseCurrentGraph(stdout.Bytes())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "terraform show JSON: " + err.Error()})
		return
	}
	graph.View = "current"
	writeJSON(w, http.StatusOK, graph)
}

func parseCurrentGraph(raw []byte) (graphResponse, error) {
	var document graphDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return graphResponse{}, err
	}
	if document.Values == nil {
		return emptyGraph(""), nil
	}
	return buildGraph(document.Values, nil), nil
}

func parsePlanGraphs(raw []byte) (*planGraphs, error) {
	var document graphDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if document.PlannedValues == nil {
		document.PlannedValues = &graphValues{}
	}
	proposed := buildGraph(document.PlannedValues, document.Configuration)
	current := emptyGraph("")
	if document.PriorState != nil && document.PriorState.Values != nil {
		current = buildGraph(document.PriorState.Values, nil)
	}
	diff := diffGraph(current, proposed, document.Changes)
	return &planGraphs{proposed: proposed, diff: diff}, nil
}

func buildGraph(values *graphValues, configuration *graphConfiguration) graphResponse {
	resources := make(map[string]graphResource)
	dependencies := make(map[string][]string)
	semantic := make(map[string][]string)
	if values != nil && values.RootModule != nil {
		collectValueResources(values.RootModule, resources, dependencies)
	}
	if configuration != nil && configuration.RootModule != nil {
		collectConfigDependencies(configuration.RootModule, "", nil, dependencies, semantic)
	}
	addresses := make([]string, 0, len(resources))
	for address := range resources {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	nodes := make([]graphNode, 0, len(addresses))
	for _, address := range addresses {
		resource := resources[address]
		nodes = append(nodes, graphNode{
			ID: address, Address: address, Type: resource.Type, Name: resource.Name, Kind: resourceKind(resource.Type),
		})
	}
	edgeSet := make(map[string]graphEdge)
	for targetReference, references := range dependencies {
		for _, target := range resolveReference(targetReference, addresses) {
			for _, reference := range references {
				for _, source := range resolveDependency(reference, target, addresses) {
					if source == target {
						continue
					}
					id := "dependency:" + source + "->" + target
					edgeSet[id] = graphEdge{ID: id, Source: source, Target: target, Kind: "dependency"}
				}
			}
		}
	}
	edges := make([]graphEdge, 0, len(edgeSet))
	for _, edge := range edgeSet {
		edges = append(edges, edge)
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	setParents(nodes, resources, semantic, addresses)
	return graphResponse{Nodes: nodes, Edges: edges}
}

func collectValueResources(module *graphModule, resources map[string]graphResource, dependencies map[string][]string) {
	for _, resource := range module.Resources {
		if resource.Mode != "managed" || resource.Address == "" {
			continue
		}
		resources[resource.Address] = resource
		dependencies[resource.Address] = append(dependencies[resource.Address], resource.DependsOn...)
	}
	for i := range module.ChildModules {
		collectValueResources(&module.ChildModules[i], resources, dependencies)
	}
}

func collectConfigDependencies(module *graphConfigModule, prefix string, inputs map[string][]string, dependencies, semantic map[string][]string) {
	for _, resource := range module.Resources {
		if resource.Mode == "data" || resource.Address == "" {
			continue
		}
		target := prefixConfigAddress(prefix, resource.Address)
		dependencies[target] = append(dependencies[target], resolveConfigReferences(prefix, resource.DependsOn, inputs)...)
		for argument, expression := range resource.Expressions {
			dependencies[target] = append(dependencies[target], resolveConfigReferences(prefix, expression.References, inputs)...)
			if argument == "vpc_id" || argument == "subnet_id" {
				semantic[target] = append(semantic[target], resolveConfigReferences(prefix, expression.References, inputs)...)
			}
			for _, references := range expression.Semantic {
				semantic[target] = append(semantic[target], resolveConfigReferences(prefix, references, inputs)...)
			}
		}
	}
	for name, call := range module.ModuleCall {
		if call.Module != nil {
			childPrefix := "module." + name
			if prefix != "" {
				childPrefix = prefix + "." + childPrefix
			}
			childInputs := make(map[string][]string, len(call.Expressions))
			for input, expression := range call.Expressions {
				childInputs[input] = resolveConfigReferences(prefix, expression.References, inputs)
			}
			collectConfigDependencies(call.Module, childPrefix, childInputs, dependencies, semantic)
		}
	}
}

func resolveConfigReferences(prefix string, references []string, inputs map[string][]string) []string {
	resolved := make([]string, 0, len(references))
	for _, reference := range references {
		if name := variableReference(reference); name != "" {
			resolved = append(resolved, inputs[name]...)
			continue
		}
		resolved = append(resolved, prefixConfigAddress(prefix, reference))
	}
	return resolved
}

func variableReference(reference string) string {
	if !strings.HasPrefix(reference, "var.") {
		return ""
	}
	name := strings.TrimPrefix(reference, "var.")
	if end := strings.IndexAny(name, ".[ "); end >= 0 {
		name = name[:end]
	}
	return name
}

func prefixConfigAddress(prefix, address string) string {
	if prefix == "" || strings.HasPrefix(address, prefix+".") {
		return address
	}
	return prefix + "." + address
}

func resolveReference(reference string, addresses []string) []string {
	exact := exactReferences(reference, addresses)
	if len(exact) != 0 {
		return exact
	}
	return expandedReferences(reference, addresses)
}

func exactReferences(reference string, addresses []string) []string {
	var exact []string
	for _, address := range addresses {
		if reference == address || strings.HasPrefix(reference, address+".") {
			exact = append(exact, address)
		}
	}
	return exact
}

func expandedReferences(reference string, addresses []string) []string {
	unindexedReference := unindexAddress(reference)
	var expanded []string
	for _, address := range addresses {
		base := unindexAddress(address)
		if unindexedReference == base || strings.HasPrefix(unindexedReference, base+".") {
			expanded = append(expanded, address)
		}
	}
	return expanded
}

func resolveDependency(reference, target string, addresses []string) []string {
	if exact := exactReferences(reference, addresses); len(exact) != 0 {
		if len(exact) == 1 {
			return exact
		}
		return nil
	}
	candidates := expandedReferences(reference, addresses)
	if len(candidates) == 0 {
		return nil
	}
	targetModule := moduleInstancePath(target)
	deepest := -1
	matching := make([]string, 0, 1)
	for _, candidate := range candidates {
		candidateModule := moduleInstancePath(candidate)
		if !modulePathPrefix(candidateModule, targetModule) {
			continue
		}
		if len(candidateModule) > deepest {
			deepest = len(candidateModule)
			matching = matching[:0]
		}
		if len(candidateModule) == deepest {
			matching = append(matching, candidate)
		}
	}
	if len(matching) == 1 {
		return matching
	}
	return nil
}

func moduleInstancePath(address string) []string {
	parts := addressSegments(address)
	var modules []string
	for len(parts) >= 2 && parts[0] == "module" {
		modules = append(modules, parts[1])
		parts = parts[2:]
	}
	return modules
}

func modulePathPrefix(candidate, target []string) bool {
	if len(candidate) > len(target) {
		return false
	}
	for i := range candidate {
		if candidate[i] != target[i] {
			return false
		}
	}
	return true
}

func addressSegments(address string) []string {
	var segments []string
	start := 0
	depth := 0
	quoted := false
	escaped := false
	for i, char := range address {
		if escaped {
			escaped = false
			continue
		}
		if quoted && char == '\\' {
			escaped = true
			continue
		}
		if char == '"' && depth > 0 {
			quoted = !quoted
			continue
		}
		if quoted {
			continue
		}
		switch char {
		case '[':
			depth++
		case ']':
			depth--
		case '.':
			if depth == 0 {
				segments = append(segments, address[start:i])
				start = i + 1
			}
		}
	}
	return append(segments, address[start:])
}

func unindexAddress(address string) string {
	var result strings.Builder
	for i := 0; i < len(address); i++ {
		if address[i] != '[' {
			result.WriteByte(address[i])
			continue
		}
		for i < len(address) && address[i] != ']' {
			i++
		}
	}
	return result.String()
}

func setParents(nodes []graphNode, resources map[string]graphResource, semantic map[string][]string, addresses []string) {
	kinds := make(map[string]string, len(nodes))
	candidates := make(map[string]map[string]struct{})
	for _, node := range nodes {
		kinds[node.ID] = node.Kind
	}
	for targetReference, references := range semantic {
		for _, target := range resolveReference(targetReference, addresses) {
			for _, reference := range references {
				for _, source := range resolveDependency(reference, target, addresses) {
					addParentCandidate(candidates, kinds, source, target)
				}
			}
		}
	}
	ids := make(map[string][]string)
	for address, resource := range resources {
		if resource.Values.ID != "" {
			ids[resource.Values.ID] = append(ids[resource.Values.ID], address)
		}
	}
	for target, resource := range resources {
		parentID := ""
		if kinds[target] == "subnet" || kinds[target] == "route-table" || kinds[target] == "internet-gateway" || kinds[target] == "security-group" {
			parentID = resource.Values.VPCID
		} else if kinds[target] == "instance" || kinds[target] == "eni" || kinds[target] == "nat-gateway" {
			parentID = resource.Values.SubnetID
		}
		for _, source := range ids[parentID] {
			addParentCandidate(candidates, kinds, source, target)
		}
	}
	for i := range nodes {
		parents := candidates[nodes[i].ID]
		if len(parents) == 1 {
			for parent := range parents {
				nodes[i].Parent = parent
			}
		}
	}
}

func addParentCandidate(candidates map[string]map[string]struct{}, kinds map[string]string, source, target string) {
	if !contains(kinds[source], kinds[target]) {
		return
	}
	if candidates[target] == nil {
		candidates[target] = make(map[string]struct{})
	}
	candidates[target][source] = struct{}{}
}

func contains(parent, child string) bool {
	if parent == "vpc" {
		return child == "subnet" || child == "route-table" || child == "internet-gateway" || child == "security-group"
	}
	return parent == "subnet" && (child == "instance" || child == "eni" || child == "nat-gateway")
}

func resourceKind(resourceType string) string {
	switch resourceType {
	case "aws_vpc":
		return "vpc"
	case "aws_subnet":
		return "subnet"
	case "aws_route_table":
		return "route-table"
	case "aws_route":
		return "route"
	case "aws_route_table_association":
		return "route-table-association"
	case "aws_internet_gateway":
		return "internet-gateway"
	case "aws_nat_gateway":
		return "nat-gateway"
	case "aws_security_group":
		return "security-group"
	case "aws_security_group_rule", "aws_vpc_security_group_ingress_rule", "aws_vpc_security_group_egress_rule":
		return "security-group-rule"
	case "aws_instance":
		return "instance"
	case "aws_network_interface":
		return "eni"
	default:
		return "generic"
	}
}

func diffGraph(current, proposed graphResponse, changes []graphChange) graphResponse {
	before := nodeMap(current.Nodes)
	after := nodeMap(proposed.Nodes)
	actions := make(map[string][]string)
	for _, change := range changes {
		if change.Mode == "managed" {
			actions[change.Address] = change.Change.Actions
		}
	}
	addresses := make(map[string]struct{}, len(before)+len(after))
	for address := range before {
		addresses[address] = struct{}{}
	}
	for address := range after {
		addresses[address] = struct{}{}
	}
	ordered := make([]string, 0, len(addresses))
	for address := range addresses {
		ordered = append(ordered, address)
	}
	sort.Strings(ordered)
	nodes := make([]graphNode, 0, len(ordered))
	for _, address := range ordered {
		node, existsAfter := after[address]
		_, existsBefore := before[address]
		if !existsAfter {
			node = before[address]
		}
		node.State = nodeState(existsBefore, existsAfter, actions[address])
		nodes = append(nodes, node)
	}

	oldEdges := edgeMap(current.Edges)
	newEdges := edgeMap(proposed.Edges)
	edgeIDs := make(map[string]struct{}, len(oldEdges)+len(newEdges))
	for id := range oldEdges {
		edgeIDs[id] = struct{}{}
	}
	for id := range newEdges {
		edgeIDs[id] = struct{}{}
	}
	orderedEdges := make([]string, 0, len(edgeIDs))
	for id := range edgeIDs {
		orderedEdges = append(orderedEdges, id)
	}
	sort.Strings(orderedEdges)
	edges := make([]graphEdge, 0, len(orderedEdges))
	for _, id := range orderedEdges {
		edge, opened := newEdges[id]
		_, closed := oldEdges[id]
		if !opened {
			edge = oldEdges[id]
		}
		switch {
		case opened && closed:
			edge.State = "unchanged"
		case opened:
			edge.State = "opened"
		default:
			edge.State = "closed"
		}
		edges = append(edges, edge)
	}
	return graphResponse{Nodes: nodes, Edges: edges}
}

func nodeState(before, after bool, actions []string) string {
	if hasAction(actions, "delete") && hasAction(actions, "create") {
		return "replaced"
	}
	if hasAction(actions, "update") {
		return "changed"
	}
	if hasAction(actions, "create") || !before && after {
		return "created"
	}
	if hasAction(actions, "delete") || before && !after {
		return "destroyed"
	}
	return "unchanged"
}

func hasAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

func nodeMap(nodes []graphNode) map[string]graphNode {
	result := make(map[string]graphNode, len(nodes))
	for _, node := range nodes {
		result[node.Address] = node
	}
	return result
}

func edgeMap(edges []graphEdge) map[string]graphEdge {
	result := make(map[string]graphEdge, len(edges))
	for _, edge := range edges {
		result[edge.ID] = edge
	}
	return result
}

func cloneGraph(graph graphResponse) graphResponse {
	graph.Nodes = append([]graphNode(nil), graph.Nodes...)
	graph.Edges = append([]graphEdge(nil), graph.Edges...)
	return graph
}

func emptyGraph(view string) graphResponse {
	return graphResponse{View: view, Nodes: []graphNode{}, Edges: []graphEdge{}}
}
