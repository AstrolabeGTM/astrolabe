package web

// Pipeline, account and conversation routes: each calls its MCP tool.
func (srv *Server) pipelineRoutes() []apiRoute {
	return []apiRoute{
		srv.toolRoute("GET /api/pipeline", "pipeline"),
		srv.toolRoute("GET /api/pipeline/people", "people_board"),
		srv.toolRoute("GET /api/pipeline/export", "pipeline_export"),
		srv.toolRoute("GET /api/account", "account"),
		srv.toolRoute("POST /api/accounts/add", "add_account"),
		srv.toolRoute("POST /api/accounts/move", "move_stage"),
		srv.toolRoute("POST /api/accounts/qualify", "qualify"),
		srv.toolRoute("POST /api/accounts/next-action", "set_next_action"),
		srv.toolRoute("POST /api/accounts/close", "close_account"),
		srv.toolRoute("POST /api/accounts/update", "update_account"),
		srv.toolRoute("GET /api/conversations", "conversations"),
		srv.toolRoute("POST /api/conversations", "log_conversation"),
	}
}
