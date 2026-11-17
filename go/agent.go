package main

// agent.go — the two agent-facing surfaces, on the same store as everything else:
//
//	POST /api/graphql   GitHub's GraphQL v4 wire: a document in, {data, errors} out. No GraphQL
//	                    library — the lexer, the recursive-descent selection parser and the executor
//	                    below cover exactly the shapes gh and github-mcp-server send.
//	POST /mcp           the Model Context Protocol over the Streamable HTTP transport, JSON-RPC 2.0,
//	                    with server/discover, initialize, tools/list and the GitHub tools an agent
//	                    drives a forge with.
//
// Two rules both keep: a protocol-level failure (an unknown field, a bad argument) is a 200 whose
// body carries the error; an absent token is the only HTTP-level failure (401).

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------------
// lexer
// ---------------------------------------------------------------------------------------------

const (
	tokName = iota
	tokNumber
	tokString
	tokPunct
)

type token struct {
	kind int
	text string
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNamePart(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// lexGraphQL turns a document into tokens.
func lexGraphQL(source string) []token {
	out := make([]token, 0, 128)
	i := 0
	for i < len(source) {
		c := source[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(source) && source[i] != '\n' {
				i++
			}
		case strings.HasPrefix(source[i:], "..."):
			out = append(out, token{tokPunct, "..."})
			i += 3
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' ||
			c == '!' || c == '$' || c == '=' || c == '@' || c == '|':
			out = append(out, token{tokPunct, string(c)})
			i++
		case c == '"':
			if strings.HasPrefix(source[i:], `"""`) {
				end := strings.Index(source[i+3:], `"""`)
				stop := len(source)
				if end >= 0 {
					stop = i + 3 + end
				}
				out = append(out, token{tokString, source[i+3 : stop]})
				i = stop + 3
				continue
			}
			j := i + 1
			var value strings.Builder
			for j < len(source) && source[j] != '"' {
				if source[j] == '\\' && j+1 < len(source) {
					value.WriteByte(source[j+1])
					j += 2
					continue
				}
				value.WriteByte(source[j])
				j++
			}
			out = append(out, token{tokString, value.String()})
			i = j + 1
		case isDigit(c) || c == '-':
			j := i
			for j < len(source) && (isDigit(source[j]) || source[j] == '.' || source[j] == 'e' || source[j] == 'E' || source[j] == '+' || source[j] == '-') {
				j++
			}
			out = append(out, token{tokNumber, source[i:j]})
			i = j
		case isNameStart(c):
			j := i
			for j < len(source) && isNamePart(source[j]) {
				j++
			}
			out = append(out, token{tokName, source[i:j]})
			i = j
		default:
			i++
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------------------------

const (
	valueVar = iota
	valueLiteral
	valueEnum
	valueList
	valueObject
)

type gqlValue struct {
	kind   int
	name   string
	lit    any
	items  []gqlValue
	fields map[string]gqlValue
}

type gqlSelection struct {
	name       string
	alias      string
	args       map[string]gqlValue
	selections []gqlSelection
	fragmentOn string
}

type gqlDocument struct {
	op         string
	selections []gqlSelection
}

type gqlParser struct {
	tokens []token
	pos    int
}

func (p *gqlParser) peek() (token, bool) {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos], true
	}
	return token{}, false
}

func (p *gqlParser) next() token {
	if p.pos >= len(p.tokens) {
		p.pos++
		return token{}
	}
	tok := p.tokens[p.pos]
	p.pos++
	return tok
}

func (p *gqlParser) expect(text string) error {
	tok := p.next()
	if tok.text != text {
		return &wireError{message: "expected " + text + ", found " + tok.text}
	}
	return nil
}

func (p *gqlParser) skipGroup(open, close string) error {
	if err := p.expect(open); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		tok := p.next()
		if tok.text == open {
			depth++
		} else if tok.text == close {
			depth--
		}
		if p.pos > len(p.tokens)+1 {
			return &wireError{message: "the document ended early"}
		}
	}
	return nil
}

func (p *gqlParser) parseDocument() (gqlDocument, error) {
	doc := gqlDocument{op: "query"}
	if head, ok := p.peek(); ok && head.kind == tokName && (head.text == "query" || head.text == "mutation") {
		doc.op = p.next().text
		if name, ok := p.peek(); ok && name.kind == tokName {
			p.next()
		}
		if brace, ok := p.peek(); ok && brace.text == "(" {
			if err := p.skipGroup("(", ")"); err != nil {
				return doc, err
			}
		}
	}
	for {
		at, ok := p.peek()
		if !ok || at.text != "@" {
			break
		}
		p.next()
		p.next()
	}
	selections, err := p.parseSelectionSet()
	doc.selections = selections
	return doc, err
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	out := make([]gqlSelection, 0, 8)
	for {
		at, ok := p.peek()
		if !ok || at.text == "}" {
			break
		}
		selection, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		out = append(out, selection)
	}
	return out, p.expect("}")
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	var selection gqlSelection
	if at, ok := p.peek(); ok && at.text == "..." {
		p.next()
		if on, ok := p.peek(); ok && on.text == "on" {
			p.next()
			selection.fragmentOn = p.next().text
		} else if name, ok := p.peek(); ok && name.kind == tokName {
			p.next()
		}
		selection.name = "..."
		selection.args = map[string]gqlValue{}
		nested, err := p.parseSelectionSet()
		if err != nil {
			return selection, err
		}
		selection.selections = nested
		return selection, nil
	}
	name := p.next().text
	if colon, ok := p.peek(); ok && colon.text == ":" {
		p.next()
		selection.alias = name
		name = p.next().text
	}
	selection.name = name
	selection.args = map[string]gqlValue{}
	if paren, ok := p.peek(); ok && paren.text == "(" {
		p.next()
		for {
			at, ok := p.peek()
			if !ok || at.text == ")" {
				break
			}
			argName := p.next().text
			if err := p.expect(":"); err != nil {
				return selection, err
			}
			value, err := p.parseValue()
			if err != nil {
				return selection, err
			}
			selection.args[argName] = value
		}
		if err := p.expect(")"); err != nil {
			return selection, err
		}
	}
	if brace, ok := p.peek(); ok && brace.text == "{" {
		nested, err := p.parseSelectionSet()
		if err != nil {
			return selection, err
		}
		selection.selections = nested
	}
	return selection, nil
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	tok := p.next()
	switch {
	case tok.text == "$":
		return gqlValue{kind: valueVar, name: p.next().text}, nil
	case tok.text == "[":
		items := make([]gqlValue, 0, 4)
		for {
			at, ok := p.peek()
			if !ok || at.text == "]" {
				break
			}
			item, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			items = append(items, item)
		}
		return gqlValue{kind: valueList, items: items}, p.expect("]")
	case tok.text == "{":
		fields := make(map[string]gqlValue, 4)
		for {
			at, ok := p.peek()
			if !ok || at.text == "}" {
				break
			}
			name := p.next().text
			if err := p.expect(":"); err != nil {
				return gqlValue{}, err
			}
			value, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			fields[name] = value
		}
		return gqlValue{kind: valueObject, fields: fields}, p.expect("}")
	case tok.kind == tokString:
		return gqlValue{kind: valueLiteral, lit: tok.text}, nil
	case tok.kind == tokNumber:
		value, err := strconv.ParseFloat(tok.text, 64)
		if err != nil {
			value = 0
		}
		return gqlValue{kind: valueLiteral, lit: value}, nil
	case tok.text == "true":
		return gqlValue{kind: valueLiteral, lit: true}, nil
	case tok.text == "false":
		return gqlValue{kind: valueLiteral, lit: false}, nil
	case tok.text == "null":
		return gqlValue{kind: valueLiteral, lit: nil}, nil
	}
	return gqlValue{kind: valueEnum, name: tok.text}, nil
}

// ---------------------------------------------------------------------------------------------
// schema and ids
// ---------------------------------------------------------------------------------------------

var gqlSchema = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "comments", "createdAt", "updatedAt", "closed"},
	"PullRequest": {"id", "number", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "title", "body"},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission",
		"hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate",
	},
	"SearchType": {},
}

var searchTypeValues = []string{"ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER", "DISCUSSION"}

func repositoryGlobalID(repo *Repo) string { return "R_" + strconv.Itoa(repo.id) }

func issueGlobalID(repo *Repo, issue *Issue) string {
	return "I_" + strconv.Itoa(repo.id) + "_" + strconv.Itoa(issue.number)
}

func pullGlobalID(repo *Repo, pull *Pull) string {
	return "P_" + strconv.Itoa(repo.id) + "_" + strconv.Itoa(pull.number)
}

// numbersIn splits a global id into its numeric parts ("I_1_2" -> [1, 2]).
func numbersIn(id any) []int {
	text, ok := id.(string)
	if !ok {
		return nil
	}
	parts := strings.Split(text, "_")
	out := make([]int, 0, len(parts))
	for _, part := range parts[1:] {
		if value, err := strconv.Atoi(part); err == nil {
			out = append(out, value)
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------------------------

type gqlError struct {
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

// gqlNode is one resolved node: a kind the field resolver understands, plus its payload.
type gqlNode struct {
	kind      string
	repo      *Repo
	issue     *Issue
	pull      *Pull
	comment   *IssueComment
	name      string
	list      []gqlNode
	hasNext   bool
	endCursor any
	value     map[string]gqlNode
}

type gqlExecutor struct {
	errors []gqlError
	forge  *Forge
	login  string
	vars   map[string]any
	origin string
}

func (e *gqlExecutor) fail(message string, path []any) any {
	entry := gqlError{Message: message}
	if path != nil {
		entry.Path = path
	}
	e.errors = append(e.errors, entry)
	return nil
}

func (e *gqlExecutor) resolve(value gqlValue) any {
	switch value.kind {
	case valueVar:
		return e.vars[value.name]
	case valueLiteral:
		return value.lit
	case valueEnum:
		return value.name
	case valueList:
		out := make([]any, 0, len(value.items))
		for _, item := range value.items {
			out = append(out, e.resolve(item))
		}
		return out
	default:
		out := make(map[string]any, len(value.fields))
		for key, item := range value.fields {
			out[key] = e.resolve(item)
		}
		return out
	}
}

func (e *gqlExecutor) arg(selection gqlSelection, name string) any {
	value, ok := selection.args[name]
	if !ok {
		return nil
	}
	return e.resolve(value)
}

// numberOf reads an argument as a number the way JavaScript's Number() does.
func numberOf(value any, fallback int) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		if parsed, err := strconv.Atoi(typed); err == nil {
			return parsed
		}
	}
	return fallback
}

func (e *gqlExecutor) object(node gqlNode, selections []gqlSelection, path []any) any {
	if node.kind == "null" {
		return nil
	}
	out := make(map[string]any, len(selections))
	for _, selection := range selections {
		if selection.fragmentOn != "" {
			if matchesFragment(node.kind, selection.fragmentOn) {
				if nested, ok := e.object(node, selection.selections, path).(map[string]any); ok {
					for key, value := range nested {
						out[key] = value
					}
				}
			}
			continue
		}
		key := selection.name
		if selection.alias != "" {
			key = selection.alias
		}
		childPath := make([]any, 0, len(path)+1)
		childPath = append(childPath, path...)
		childPath = append(childPath, key)
		out[key] = e.field(node, selection, childPath)
	}
	return out
}

func (e *gqlExecutor) field(node gqlNode, selection gqlSelection, path []any) any {
	name := selection.name
	repo := node.repo
	issue := node.issue
	pull := node.pull
	switch node.kind {
	case "user":
		switch name {
		case "login", "name":
			return e.login
		case "id":
			return "U_" + e.login
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'User'", path)
	case "repo":
		if repo == nil {
			return nil
		}
		switch name {
		case "id":
			return repositoryGlobalID(repo)
		case "databaseId":
			return repo.id
		case "name":
			return repo.name
		case "nameWithOwner":
			return repo.owner + "/" + repo.name
		case "description":
			return repo.description
		case "isPrivate":
			return repo.isPrivate
		case "owner":
			return e.object(gqlNode{kind: "user", repo: repo}, selection.selections, path)
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "defaultBranchRef":
			if _, ok := repo.refs["refs/heads/"+defaultBranch]; !ok {
				return nil
			}
			return e.object(gqlNode{kind: "ref", repo: repo}, selection.selections, path)
		case "issue":
			found := repo.issues[numberOf(e.arg(selection, "number"), 0)]
			if found == nil {
				return nil
			}
			return e.object(gqlNode{kind: "issue", repo: repo, issue: found}, selection.selections, path)
		case "issues":
			return e.issuesConnection(repo, selection, path)
		case "pullRequest":
			found := repo.pulls[numberOf(e.arg(selection, "number"), 0)]
			if found == nil {
				return nil
			}
			return e.object(gqlNode{kind: "pr", repo: repo, pull: found}, selection.selections, path)
		case "pullRequests":
			first := numberOf(e.arg(selection, "first"), 30)
			open := make([]*Pull, 0, len(repo.pulls))
			for _, entry := range repo.pulls {
				if entry.state == "open" {
					open = append(open, entry)
				}
			}
			sort.SliceStable(open, func(i, j int) bool { return open[i].number > open[j].number })
			if first < len(open) {
				open = open[:first]
			}
			list := make([]gqlNode, 0, len(open))
			for _, entry := range open {
				list = append(list, gqlNode{kind: "pr", repo: repo, pull: entry})
			}
			return e.object(gqlNode{kind: "prConn", repo: repo, list: list}, selection.selections, path)
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'Repository'", path)
	case "ref":
		if name == "name" {
			if repo != nil {
				return defaultBranch
			}
			return nil
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'Ref'", path)
	case "issue":
		if issue == nil {
			return nil
		}
		switch name {
		case "id":
			return issueGlobalID(repo, issue)
		case "number":
			return issue.number
		case "title":
			return issue.title
		case "body":
			return issue.body
		case "state":
			if issue.state == "open" {
				return "OPEN"
			}
			return "CLOSED"
		case "closed":
			return issue.state != "open"
		case "createdAt", "updatedAt":
			return stateEpochISO
		case "comments":
			list := make([]gqlNode, 0, len(issue.comments))
			for i := range issue.comments {
				list = append(list, gqlNode{kind: "comment", comment: &issue.comments[i]})
			}
			return e.object(gqlNode{kind: "commentConn", list: list}, selection.selections, path)
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'Issue'", path)
	case "pr":
		if pull == nil {
			return nil
		}
		switch name {
		case "id":
			return pullGlobalID(repo, pull)
		case "number":
			return pull.number
		case "title":
			return pull.title
		case "body":
			return pull.body
		case "state":
			if pull.merged {
				return "MERGED"
			}
			if pull.state == "open" {
				return "OPEN"
			}
			return "CLOSED"
		case "merged":
			return pull.merged
		case "mergeable":
			return "MERGEABLE"
		case "headRefName":
			return pull.headRef
		case "baseRefName":
			return pull.baseRef
		case "files":
			paths := gqlPullFilePaths(repo, pull)
			first := numberOf(e.arg(selection, "first"), 100)
			if first < len(paths) {
				paths = paths[:first]
			}
			list := make([]gqlNode, 0, len(paths))
			for _, pathName := range paths {
				list = append(list, gqlNode{kind: "file", name: pathName})
			}
			return e.object(gqlNode{kind: "fileConn", list: list}, selection.selections, path)
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'PullRequest'", path)
	case "file":
		if name == "path" {
			return node.name
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'PullRequestChangedFile'", path)
	case "fileConn", "prConn", "commentConn":
		list := node.list
		switch name {
		case "nodes":
			out := make([]any, 0, len(list))
			for _, entry := range list {
				out = append(out, e.object(entry, selection.selections, path))
			}
			return out
		case "totalCount":
			return len(list)
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'Connection'", path)
	case "issueConn":
		list := node.list
		switch name {
		case "nodes":
			out := make([]any, 0, len(list))
			for _, entry := range list {
				out = append(out, e.object(entry, selection.selections, path))
			}
			return out
		case "totalCount":
			return len(list)
		case "pageInfo":
			return e.object(gqlNode{kind: "pageInfo", hasNext: node.hasNext, endCursor: node.endCursor}, selection.selections, path)
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'IssueConnection'", path)
	case "pageInfo":
		switch name {
		case "hasNextPage":
			return node.hasNext
		case "endCursor", "startCursor":
			return node.endCursor
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'PageInfo'", path)
	case "comment":
		switch name {
		case "id":
			if node.comment != nil {
				return "C_" + strconv.Itoa(node.comment.id)
			}
			return "C_0"
		case "body":
			if node.comment != nil {
				return node.comment.body
			}
			return ""
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'IssueComment'", path)
	case "commentEdge":
		if name == "node" {
			return e.object(gqlNode{kind: "comment", comment: node.comment}, selection.selections, path)
		}
		return e.fail("Field '"+name+"' doesn't exist on type 'IssueCommentEdge'", path)
	case "type":
		typeName := node.name
		switch name {
		case "name":
			return typeName
		case "fields", "enumValues":
			var values []string
			if name == "fields" {
				values = gqlSchema[typeName]
			} else if typeName == "SearchType" {
				values = searchTypeValues
			}
			out := make([]any, 0, len(values))
			for _, entry := range values {
				out = append(out, e.object(gqlNode{kind: "field", name: entry}, selection.selections, path))
			}
			return out
		}
		return e.fail("Field '"+name+"' doesn't exist on type '__Type'", path)
	case "field":
		if name == "name" {
			return node.name
		}
		return e.fail("Field '"+name+"' doesn't exist on type '__Field'", path)
	case "payload":
		payload := node.value
		if payload == nil {
			return nil
		}
		child, ok := payload[name]
		if !ok {
			return e.fail("Field '"+name+"' doesn't exist on type 'Payload'", path)
		}
		return e.object(child, selection.selections, path)
	}
	return nil
}

// issuesConnection is issues(first, states, orderBy, after) as a connection node.
func (e *gqlExecutor) issuesConnection(repo *Repo, selection gqlSelection, path []any) any {
	first := numberOf(e.arg(selection, "first"), 30)
	states := e.arg(selection, "states")
	var wanted []string
	all := false
	if list, ok := states.([]any); ok {
		for _, entry := range list {
			value, _ := entry.(string)
			if value == "ALL" {
				all = true
			}
			wanted = append(wanted, value)
		}
	}
	after := e.arg(selection, "after")
	direction := "DESC"
	if orderBy, ok := e.arg(selection, "orderBy").(map[string]any); ok {
		if value, ok := orderBy["direction"].(string); ok {
			direction = value
		}
	}
	includes := func(state string) bool {
		for _, value := range wanted {
			if value == state {
				return true
			}
		}
		return false
	}
	list := make([]*Issue, 0, len(repo.issues))
	for _, issue := range repo.issues {
		if len(wanted) > 0 && !all {
			if (includes("OPEN") && issue.state == "open") || (includes("CLOSED") && issue.state == "closed") {
				list = append(list, issue)
			}
			continue
		}
		list = append(list, issue)
	}
	ascending := direction == "ASC"
	sort.SliceStable(list, func(i, j int) bool {
		if ascending {
			return list[i].number < list[j].number
		}
		return list[i].number > list[j].number
	})
	if cursor, ok := after.(string); ok && cursor != "" {
		if value, err := strconv.Atoi(cursor); err == nil {
			kept := list[:0]
			for _, issue := range list {
				if ascending && issue.number > value {
					kept = append(kept, issue)
				} else if !ascending && issue.number < value {
					kept = append(kept, issue)
				}
			}
			list = kept
		}
	}
	page := list
	if first < len(page) {
		page = page[:first]
	}
	var endCursor any
	if len(page) > 0 {
		endCursor = strconv.Itoa(page[len(page)-1].number)
	}
	listNodes := make([]gqlNode, 0, len(page))
	for _, issue := range page {
		listNodes = append(listNodes, gqlNode{kind: "issue", repo: repo, issue: issue})
	}
	return e.object(gqlNode{kind: "issueConn", repo: repo, list: listNodes, hasNext: len(list) > len(page), endCursor: endCursor}, selection.selections, path)
}

// repoById answers a repository by global id.
func (e *gqlExecutor) repoById(id any) *Repo {
	numbers := numbersIn(id)
	if len(numbers) == 0 {
		return nil
	}
	return e.forge.findById(numbers[0])
}

func (e *gqlExecutor) findIssue(id any) (*Repo, *Issue) {
	numbers := numbersIn(id)
	if len(numbers) < 2 {
		return nil, nil
	}
	repo := e.forge.findById(numbers[0])
	if repo == nil {
		return nil, nil
	}
	issue := repo.issues[numbers[1]]
	if issue == nil {
		return nil, nil
	}
	return repo, issue
}

func (e *gqlExecutor) findPull(id any) (*Repo, *Pull) {
	numbers := numbersIn(id)
	if len(numbers) < 2 {
		return nil, nil
	}
	repo := e.forge.findById(numbers[0])
	if repo == nil {
		return nil, nil
	}
	pull := repo.pulls[numbers[1]]
	if pull == nil {
		return nil, nil
	}
	return repo, pull
}

// mutate executes a mutation document's selections.
func (e *gqlExecutor) mutate(selections []gqlSelection) map[string]any {
	out := make(map[string]any, len(selections))
	for _, selection := range selections {
		key := selection.name
		if selection.alias != "" {
			key = selection.alias
		}
		input, _ := e.arg(selection, "input").(map[string]any)
		if input == nil {
			input = map[string]any{}
		}
		switch selection.name {
		case "createIssue":
			repo := e.repoById(input["repositoryId"])
			if repo == nil {
				out[key] = e.fail("Could not resolve to a Repository with the global id", []any{key})
				continue
			}
			number := repo.nextIssue
			repo.nextIssue++
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			issue := &Issue{number: number, id: e.forge.issueID(), title: title, body: body, state: "open", user: e.login}
			repo.issues[number] = issue
			out[key] = e.object(gqlNode{kind: "payload", value: map[string]gqlNode{"issue": {kind: "issue", repo: repo, issue: issue}}}, selection.selections, []any{key})
		case "closeIssue":
			repo, issue := e.findIssue(input["issueId"])
			if issue == nil {
				out[key] = e.fail("Could not resolve to an Issue with the global id", []any{key})
				continue
			}
			issue.state = "closed"
			out[key] = e.object(gqlNode{kind: "payload", value: map[string]gqlNode{"issue": {kind: "issue", repo: repo, issue: issue}}}, selection.selections, []any{key})
		case "createPullRequest":
			repo := e.repoById(input["repositoryId"])
			headRef, _ := input["headRefName"].(string)
			baseRef, _ := input["baseRefName"].(string)
			if repo == nil {
				out[key] = e.fail("Could not resolve the head or base ref", []any{key})
				continue
			}
			headSha, headOk := repo.refs["refs/heads/"+headRef]
			baseSha, baseOk := repo.refs["refs/heads/"+baseRef]
			if !headOk || !baseOk {
				out[key] = e.fail("Could not resolve the head or base ref", []any{key})
				continue
			}
			number := repo.nextPull
			repo.nextPull++
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			pull := &Pull{number: number, id: e.forge.pullID(), title: title, body: body, state: "open", headRef: headRef, headSha: headSha, baseRef: baseRef, baseSha: baseSha}
			repo.pulls[number] = pull
			out[key] = e.object(gqlNode{kind: "payload", value: map[string]gqlNode{"pullRequest": {kind: "pr", repo: repo, pull: pull}}}, selection.selections, []any{key})
		case "mergePullRequest":
			repo, pull := e.findPull(input["pullRequestId"])
			if pull == nil || pull.merged {
				out[key] = e.fail("Pull request is not mergeable", []any{key})
				continue
			}
			mergePullRequest(repo, pull, e.login)
			out[key] = e.object(gqlNode{kind: "payload", value: map[string]gqlNode{"pullRequest": {kind: "pr", repo: repo, pull: pull}}}, selection.selections, []any{key})
		case "addComment":
			repo, issue := e.findIssue(input["subjectId"])
			if issue == nil {
				out[key] = e.fail("Could not resolve to an Issue with the global id", []any{key})
				continue
			}
			body, _ := input["body"].(string)
			comment := IssueComment{id: repo.nextCommentID, body: body, user: e.login}
			repo.nextCommentID++
			issue.comments = append(issue.comments, comment)
			stored := &issue.comments[len(issue.comments)-1]
			out[key] = e.object(gqlNode{kind: "payload", value: map[string]gqlNode{"commentEdge": {kind: "commentEdge", comment: stored}}}, selection.selections, []any{key})
		default:
			out[key] = e.fail("Field '"+selection.name+"' doesn't exist on type 'Mutation'", []any{key})
		}
	}
	return out
}

func matchesFragment(kind, onType string) bool {
	switch onType {
	case "Repository":
		return kind == "repo" || kind == "node"
	case "Issue":
		return kind == "issue" || kind == "node"
	case "PullRequest":
		return kind == "pr" || kind == "node"
	case "User":
		return kind == "user"
	}
	return kind == "node"
}

// gqlPullFilePaths is the changed paths of a pull request, base tree to head tree.
func gqlPullFilePaths(repo *Repo, pull *Pull) []string {
	baseTree, hasBase := commitTree(repo, pull.baseSha, true)
	headTree, hasHead := commitTree(repo, pull.headSha, true)
	diff := diffTrees(repo, baseTree, hasBase, headTree, hasHead)
	paths := make([]string, 0, len(diff.added)+len(diff.modified)+len(diff.removed))
	paths = append(paths, diff.added...)
	paths = append(paths, diff.modified...)
	paths = append(paths, diff.removed...)
	sort.Strings(paths)
	return paths
}

// executeDocument runs one operation document against the forge.
func executeDocument(document gqlDocument, forge *Forge, login, origin string, variables map[string]any) map[string]any {
	executor := &gqlExecutor{forge: forge, login: login, vars: variables, origin: origin}
	if document.op == "mutation" {
		data := executor.mutate(document.selections)
		out := map[string]any{"data": data}
		if len(executor.errors) > 0 {
			out["errors"] = executor.errors
		}
		return out
	}
	data := make(map[string]any, len(document.selections))
	for _, selection := range document.selections {
		key := selection.name
		if selection.alias != "" {
			key = selection.alias
		}
		switch selection.name {
		case "viewer":
			data[key] = executor.object(gqlNode{kind: "user"}, selection.selections, nil)
		case "repository":
			owner, _ := executor.arg(selection, "owner").(string)
			name, _ := executor.arg(selection, "name").(string)
			repo := forge.get([]byte(owner), []byte(name))
			if repo == nil {
				data[key] = nil
			} else {
				data[key] = executor.object(gqlNode{kind: "repo", repo: repo}, selection.selections, nil)
			}
		case "node":
			repo := executor.repoById(executor.arg(selection, "id"))
			if repo == nil {
				data[key] = nil
			} else {
				data[key] = executor.object(gqlNode{kind: "repo", repo: repo}, selection.selections, nil)
			}
		case "__type":
			typeName, _ := executor.arg(selection, "name").(string)
			if _, exists := gqlSchema[typeName]; !exists {
				data[key] = nil
			} else {
				data[key] = executor.object(gqlNode{kind: "type", name: typeName}, selection.selections, nil)
			}
		default:
			data[key] = nil
			executor.errors = append(executor.errors, gqlError{Message: "Field '" + selection.name + "' doesn't exist on type 'Query'", Path: []any{key}})
		}
	}
	out := map[string]any{"data": data}
	if len(executor.errors) > 0 {
		out["errors"] = executor.errors
	}
	return out
}

// handleGraphQL answers POST /api/graphql.
func (s *Server) handleGraphQL(req *request) response {
	if req.method != methodPost {
		return messageJSON(404, "Not Found")
	}
	login, authed := s.loginOf(req.auth, req.hasAuth)
	if !authed {
		return messageJSON(401, "Requires authentication")
	}
	var parsed any
	if err := json.Unmarshal(req.body, &parsed); err != nil {
		return messageJSON(400, "Problems parsing JSON")
	}
	origin := "http://127.0.0.1"
	if len(req.host) > 0 {
		origin = "http://" + string(req.host)
	}
	run := func(operation any) map[string]any {
		entry, _ := operation.(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		document := ""
		if query, ok := entry["query"].(string); ok {
			document = query
		}
		variables := map[string]any{}
		if given, ok := entry["variables"].(map[string]any); ok {
			variables = given
		}
		parsedDocument, err := parseGraphQL(document)
		if err != nil {
			return map[string]any{"data": nil, "errors": []gqlError{{Message: err.Error()}}}
		}
		return executeDocument(parsedDocument, s.forge, login, origin, variables)
	}
	if operations, ok := parsed.([]any); ok {
		answers := make([]any, 0, len(operations))
		for _, operation := range operations {
			answers = append(answers, run(operation))
		}
		return jsonReply(200, marshalJSONValue(answers))
	}
	return jsonReply(200, marshalJSONValue(run(parsed)))
}

// parseGraphQL parses one document's text into the operation the executor runs.
func parseGraphQL(query string) (document gqlDocument, err error) {
	parser := &gqlParser{tokens: lexGraphQL(query)}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &wireError{message: "the document could not be parsed"}
		}
	}()
	return parser.parseDocument()
}

// marshalJSONValue serialises a value the way every reply does (one trailing newline, no HTML
// escaping: a body an agent reads is JSON, not a web page).
func marshalJSONValue(value any) []byte {
	writer := &bytes.Buffer{}
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return writer.Bytes()
}

// marshalJSONCompact is the same without the trailing newline (a text block inside a reply).
func marshalJSONCompact(value any) []byte {
	body := marshalJSONValue(value)
	return bytes.TrimSuffix(body, []byte("\n"))
}

// ---------------------------------------------------------------------------------------------
// MCP
// ---------------------------------------------------------------------------------------------

const (
	mcpPreferredRevision = "2026-07-28"
	jsonRPCParseError    = -32700
	jsonRPCInvalidReq    = -32600
	jsonRPCMethodMissing = -32601
	jsonRPCInvalidParams = -32602
)

var mcpRevisions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var mcpServerInfo = map[string]any{
	"name":    "ghforge-mcp",
	"version": "1.0.0",
	"title":   "GitHub forge MCP server",
}

// mcpToolsJSON is the tools/list result's payload, built once so two answers match byte for byte.
var mcpToolsJSON = func() []byte {
	text := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	boolean := func(description string) map[string]any {
		return map[string]any{"type": "boolean", "description": description}
	}
	integer := func(description string) map[string]any {
		return map[string]any{"type": "integer", "description": description}
	}
	schema := func(properties map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	tools := []any{
		map[string]any{"name": "create_repo", "title": "Create repository", "description": "Create a repository owned by the authenticated account.",
			"inputSchema": schema(map[string]any{"name": text("Repository name"), "description": text("Repository description"), "private": boolean("Whether the repository is private")}, []string{"name"})},
		map[string]any{"name": "get_repo", "title": "Get repository", "description": "Read one repository by owner and name.",
			"inputSchema": schema(map[string]any{"owner": text("Repository owner login"), "repo": text("Repository name")}, []string{"owner", "repo"})},
		map[string]any{"name": "list_branches", "title": "List branches", "description": "List the branches of a repository with the commit each one points at.",
			"inputSchema": schema(map[string]any{"owner": text("Repository owner login"), "repo": text("Repository name")}, []string{"owner", "repo"})},
		map[string]any{"name": "create_issue", "title": "Create issue", "description": "Open an issue in a repository.",
			"inputSchema": schema(map[string]any{"owner": text("Repository owner login"), "repo": text("Repository name"), "title": text("Issue title"), "body": text("Issue body")}, []string{"owner", "repo", "title"})},
		map[string]any{"name": "list_issues", "title": "List issues", "description": "List the issues of a repository, optionally filtered by state.",
			"inputSchema": schema(map[string]any{"owner": text("Repository owner login"), "repo": text("Repository name"), "state": text("open, closed or all")}, []string{"owner", "repo"})},
		map[string]any{"name": "add_issue_comment", "title": "Add issue comment", "description": "Add a comment to an existing issue.",
			"inputSchema": schema(map[string]any{"owner": text("Repository owner login"), "repo": text("Repository name"), "issue_number": integer("Issue number"), "body": text("Comment body")}, []string{"owner", "repo", "issue_number", "body"})},
		map[string]any{"name": "create_pull_request", "title": "Create pull request", "description": "Open a pull request from a head branch into a base branch.",
			"inputSchema": schema(map[string]any{"owner": text("Repository owner login"), "repo": text("Repository name"), "title": text("Pull request title"), "body": text("Pull request body"), "head": text("Head branch"), "base": text("Base branch")}, []string{"owner", "repo", "title", "head", "base"})},
		map[string]any{"name": "list_pull_requests", "title": "List pull requests", "description": "List the pull requests of a repository.",
			"inputSchema": schema(map[string]any{"owner": text("Repository owner login"), "repo": text("Repository name"), "state": text("open, closed or all")}, []string{"owner", "repo"})},
		map[string]any{"name": "get_pull_request", "title": "Get pull request", "description": "Read one pull request by number.",
			"inputSchema": schema(map[string]any{"owner": text("Repository owner login"), "repo": text("Repository name"), "pull_number": integer("Pull request number")}, []string{"owner", "repo", "pull_number"})},
		map[string]any{"name": "merge_pull_request", "title": "Merge pull request", "description": "Merge a pull request into its base branch.",
			"inputSchema": schema(map[string]any{"owner": text("Repository owner login"), "repo": text("Repository name"), "pull_number": integer("Pull request number"), "merge_method": text("merge, squash or rebase")}, []string{"owner", "repo", "pull_number"})},
	}
	return marshalJSONCompact(map[string]any{"tools": tools})
}()

func rpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func textResult(value any) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(marshalJSONCompact(value))}},
		"structuredContent": value,
		"isError":           false,
	}
}

func toolError(message string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}, "isError": true}
}

func mcpArgString(args map[string]any, key string) (string, bool) {
	if value, ok := args[key].(string); ok && value != "" {
		return value, true
	}
	return "", false
}

func mcpArgInt(args map[string]any, key string) (int, bool) {
	switch value := args[key].(type) {
	case float64:
		if value == float64(int(value)) {
			return int(value), true
		}
	case int:
		return value, true
	case string:
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

// mcpRepoView is one repository as an agent addresses it again.
func mcpRepoView(repo *Repo) map[string]any {
	branches := make([]string, 0, len(repo.refs))
	for ref := range repo.refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			branches = append(branches, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(branches)
	return map[string]any{
		"id":             repo.id,
		"name":           repo.name,
		"full_name":      repo.owner + "/" + repo.name,
		"owner":          repo.owner,
		"description":    repo.description,
		"private":        repo.isPrivate,
		"default_branch": defaultBranch,
		"branches":       branches,
		"open_issues":    len(repo.issues),
		"html_url":       "/" + repo.owner + "/" + repo.name,
	}
}

func mcpIssueView(issue *Issue) map[string]any {
	labels := issue.labels
	if labels == nil {
		labels = []string{}
	}
	return map[string]any{
		"id":         issue.id,
		"number":     issue.number,
		"title":      issue.title,
		"body":       issue.body,
		"state":      issue.state,
		"labels":     labels,
		"comments":   len(issue.comments),
		"created_at": stateEpochISO,
	}
}

func mcpPullView(pull *Pull) map[string]any {
	state := pull.state
	if pull.merged {
		state = "merged"
	}
	out := map[string]any{
		"id":               pull.id,
		"number":           pull.number,
		"title":            pull.title,
		"body":             pull.body,
		"state":            state,
		"merged":           pull.merged,
		"head":             map[string]any{"ref": pull.headRef, "sha": pull.headSha.hex()},
		"base":             map[string]any{"ref": pull.baseRef, "sha": pull.baseSha.hex()},
		"merge_commit_sha": nil,
	}
	if pull.mergeCommitSha != (oid{}) {
		out["merge_commit_sha"] = pull.mergeCommitSha.hex()
	}
	return out
}

// callTool runs one tool; the answer is either a result or a JSON-RPC error code and message.
func (s *Server) callTool(login, name string, args map[string]any) (result any, code int, message string) {
	owner := login
	if value, ok := mcpArgString(args, "owner"); ok {
		owner = value
	}
	repoName, hasRepoName := mcpArgString(args, "repo")
	findRepo := func() *Repo {
		if !hasRepoName {
			return nil
		}
		return s.forge.get([]byte(owner), []byte(repoName))
	}
	missingRepo := func() (any, int, string) {
		return nil, jsonRPCInvalidParams, "no repository " + owner + "/" + repoName
	}

	switch name {
	case "create_repo":
		created, ok := mcpArgString(args, "name")
		if !ok {
			return nil, jsonRPCInvalidParams, "create_repo needs a name"
		}
		if existing := s.forge.get([]byte(login), []byte(created)); existing != nil {
			return textResult(mcpRepoView(existing)), 0, ""
		}
		description, _ := mcpArgString(args, "description")
		isPrivate, _ := args["private"].(bool)
		repo := s.forge.create(login, created, description, isPrivate)
		if repo == nil {
			return nil, jsonRPCInvalidParams, "the repository " + login + "/" + created + " could not be created"
		}
		return textResult(mcpRepoView(repo)), 0, ""
	case "get_repo":
		repo := findRepo()
		if repo == nil {
			return missingRepo()
		}
		return textResult(mcpRepoView(repo)), 0, ""
	case "list_branches":
		repo := findRepo()
		if repo == nil {
			return missingRepo()
		}
		names := make([]string, 0, len(repo.refs))
		for ref := range repo.refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, ref)
			}
		}
		sort.Strings(names)
		branches := make([]any, 0, len(names))
		for _, ref := range names {
			branches = append(branches, map[string]any{"name": strings.TrimPrefix(ref, "refs/heads/"), "sha": repo.refs[ref].hex()})
		}
		return textResult(map[string]any{"branches": branches}), 0, ""
	case "create_issue":
		repo := findRepo()
		if repo == nil {
			return missingRepo()
		}
		title, ok := mcpArgString(args, "title")
		if !ok {
			return nil, jsonRPCInvalidParams, "create_issue needs a title"
		}
		body, _ := mcpArgString(args, "body")
		number := repo.nextIssue
		repo.nextIssue++
		issue := &Issue{number: number, id: s.forge.issueID(), title: title, body: body, state: "open", user: login}
		repo.issues[number] = issue
		return textResult(mcpIssueView(issue)), 0, ""
	case "list_issues":
		repo := findRepo()
		if repo == nil {
			return missingRepo()
		}
		wanted, _ := mcpArgString(args, "state")
		if wanted == "" {
			wanted = "open"
		}
		list := make([]*Issue, 0, len(repo.issues))
		for _, issue := range repo.issues {
			if wanted == "all" || issue.state == wanted {
				list = append(list, issue)
			}
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].number > list[j].number })
		issues := make([]any, 0, len(list))
		for _, issue := range list {
			issues = append(issues, mcpIssueView(issue))
		}
		return textResult(map[string]any{"total": len(issues), "issues": issues}), 0, ""
	case "add_issue_comment":
		repo := findRepo()
		if repo == nil {
			return missingRepo()
		}
		number, ok := mcpArgInt(args, "issue_number")
		if !ok || repo.issues[number] == nil {
			return nil, jsonRPCInvalidParams, "add_issue_comment needs an existing issue_number"
		}
		body, ok := mcpArgString(args, "body")
		if !ok {
			return nil, jsonRPCInvalidParams, "add_issue_comment needs a body"
		}
		issue := repo.issues[number]
		comment := IssueComment{id: repo.nextCommentID, body: body, user: login}
		repo.nextCommentID++
		issue.comments = append(issue.comments, comment)
		return textResult(map[string]any{"id": comment.id, "body": comment.body, "issue_number": issue.number}), 0, ""
	case "create_pull_request":
		repo := findRepo()
		if repo == nil {
			return missingRepo()
		}
		head, hasHead := mcpArgString(args, "head")
		base, hasBase := mcpArgString(args, "base")
		title, hasTitle := mcpArgString(args, "title")
		if !hasHead || !hasBase || !hasTitle {
			return nil, jsonRPCInvalidParams, "create_pull_request needs head, base and title"
		}
		headSha, headOk := repo.refs["refs/heads/"+head]
		baseSha, baseOk := repo.refs["refs/heads/"+base]
		if !headOk || !baseOk {
			return nil, jsonRPCInvalidParams, "the branches " + head + " and " + base + " must both exist"
		}
		body, _ := mcpArgString(args, "body")
		number := repo.nextPull
		repo.nextPull++
		pull := &Pull{number: number, id: s.forge.pullID(), title: title, body: body, state: "open", headRef: head, headSha: headSha, baseRef: base, baseSha: baseSha}
		repo.pulls[number] = pull
		return textResult(mcpPullView(pull)), 0, ""
	case "list_pull_requests":
		repo := findRepo()
		if repo == nil {
			return missingRepo()
		}
		wanted, _ := mcpArgString(args, "state")
		if wanted == "" {
			wanted = "open"
		}
		list := make([]*Pull, 0, len(repo.pulls))
		for _, pull := range repo.pulls {
			if wanted == "all" || (wanted == "open" && pull.state == "open") || (wanted != "open" && pull.state == "closed") {
				list = append(list, pull)
			}
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].number > list[j].number })
		pulls := make([]any, 0, len(list))
		for _, pull := range list {
			pulls = append(pulls, mcpPullView(pull))
		}
		return textResult(map[string]any{"total": len(pulls), "pull_requests": pulls}), 0, ""
	case "get_pull_request":
		repo := findRepo()
		if repo == nil {
			return missingRepo()
		}
		number, ok := mcpArgInt(args, "pull_number")
		if !ok || repo.pulls[number] == nil {
			return nil, jsonRPCInvalidParams, "get_pull_request needs an existing pull_number"
		}
		return textResult(mcpPullView(repo.pulls[number])), 0, ""
	case "merge_pull_request":
		repo := findRepo()
		if repo == nil {
			return missingRepo()
		}
		number, ok := mcpArgInt(args, "pull_number")
		if !ok || repo.pulls[number] == nil {
			return nil, jsonRPCInvalidParams, "merge_pull_request needs an existing pull_number"
		}
		pull := repo.pulls[number]
		mergeSha, merged := mergePullRequest(repo, pull, login)
		if !merged {
			return toolError("pull request #" + strconv.Itoa(pull.number) + " is not mergeable"), 0, ""
		}
		view := mcpPullView(pull)
		view["merge_commit_sha"] = mergeSha.hex()
		view["merged"] = true
		view["state"] = "merged"
		return textResult(view), 0, ""
	}
	return nil, jsonRPCInvalidParams, "unknown tool " + strconv.Quote(name)
}

// handleMCP answers POST /mcp: a JSON-RPC request, a notification, or a body that is not JSON.
func (s *Server) handleMCP(req *request) response {
	if req.method == methodDelete {
		return response{status: 204, body: emptyBytes}
	}
	if req.method != methodPost {
		return messageJSON(405, "Method Not Allowed")
	}
	login, authed := s.loginOf(req.auth, req.hasAuth)
	if !authed {
		return jsonReply(401, marshalJSONValue(rpcError(nil, jsonRPCInvalidReq, "Authorization is required")))
	}
	var decoded any
	if err := json.Unmarshal(req.body, &decoded); err != nil {
		return jsonReply(400, marshalJSONValue(rpcError(nil, jsonRPCParseError, "Parse error: the body is not JSON")))
	}
	message, ok := decoded.(map[string]any)
	if !ok {
		return jsonReply(400, marshalJSONValue(rpcError(nil, jsonRPCInvalidReq, "Invalid Request")))
	}
	rawID, hasID := message["id"]
	id := rawID
	if !hasID || id == nil {
		id = nil
	}
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	// A notification has no id and takes no answer body.
	if !hasID {
		return response{status: 202, body: emptyBytes}
	}

	switch method {
	case "server/discover":
		return jsonReply(200, marshalJSONValue(rpcResult(id, map[string]any{
			"resultType":        "complete",
			"supportedVersions": mcpRevisions,
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":        mcpServerInfo,
			"instructions":      "A GitHub-compatible forge: repositories, issues and pull requests.",
		})))
	case "initialize":
		asked, _ := params["protocolVersion"].(string)
		version := mcpPreferredRevision
		for _, supported := range mcpRevisions {
			if supported == asked {
				version = asked
				break
			}
		}
		return jsonReply(200, marshalJSONValue(rpcResult(id, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      mcpServerInfo,
		})))
	case "ping":
		return jsonReply(200, marshalJSONValue(rpcResult(id, map[string]any{})))
	case "tools/list":
		body := make([]byte, 0, len(mcpToolsJSON)+96)
		body = append(body, `{"jsonrpc":"2.0","id":`...)
		body = append(body, marshalJSONCompact(id)...)
		body = append(body, `,"result":`...)
		body = append(body, mcpToolsJSON...)
		body = append(body, '}', '\n')
		return jsonReply(200, body)
	case "tools/call":
		name, _ := params["name"].(string)
		if name == "" {
			return jsonReply(200, marshalJSONValue(rpcError(id, jsonRPCInvalidParams, "tools/call needs a tool name")))
		}
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result, code, problem := s.callTool(login, name, args)
		if code != 0 {
			return jsonReply(200, marshalJSONValue(rpcError(id, code, problem)))
		}
		return jsonReply(200, marshalJSONValue(rpcResult(id, result)))
	case "resources/list":
		return jsonReply(200, marshalJSONValue(rpcResult(id, map[string]any{"resources": []any{}})))
	case "prompts/list":
		return jsonReply(200, marshalJSONValue(rpcResult(id, map[string]any{"prompts": []any{}})))
	}
	return jsonReply(200, marshalJSONValue(rpcError(id, jsonRPCMethodMissing, "Method not found: "+strconv.Quote(method))))
}
