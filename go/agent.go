package main

// agent.go — the agent-facing surfaces: GraphQL v4 at /api/graphql and MCP at /mcp.
//
// Phase A leaves them answering the same JSON 404 as any unknown route; the REST and git arms are
// what this phase ports. Phase B fills these in on the same store.

// handleGraphQL answers POST /api/graphql.
func (s *Server) handleGraphQL(req *request) response {
	return messageJSON(404, "Not Found")
}

// handleMCP answers POST /mcp.
func (s *Server) handleMCP(req *request) response {
	return messageJSON(404, "Not Found")
}
