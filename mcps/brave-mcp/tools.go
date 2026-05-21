package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolHandlers struct {
	client *braveClient
}

func NewToolHandlers(client *braveClient) *ToolHandlers {
	return &ToolHandlers{client: client}
}

type WebSearchInput struct {
	Query      string `json:"query" jsonschema:"required,description=Search query"`
	Count      int    `json:"count,omitempty" jsonschema:"description=Number of results (1-20, default 10)"`
	Offset     int    `json:"offset,omitempty" jsonschema:"description=Pagination offset (0-9)"`
	Safesearch string `json:"safesearch,omitempty" jsonschema:"description=Filter: off, moderate, or strict"`
	Freshness  string `json:"freshness,omitempty" jsonschema:"description=Age filter: pd, pw, pm, py, or YYYY-MM-DDtoYYYY-MM-DD"`
}

func (h *ToolHandlers) handleWebSearch(ctx context.Context, req *mcp.CallToolRequest, input WebSearchInput) (*mcp.CallToolResult, any, error) {
	params := url.Values{}
	params.Set("q", input.Query)
	if input.Count > 0 {
		params.Set("count", fmt.Sprintf("%d", input.Count))
	}
	if input.Offset > 0 {
		params.Set("offset", fmt.Sprintf("%d", input.Offset))
	}
	if input.Safesearch != "" {
		params.Set("safesearch", input.Safesearch)
	}
	if input.Freshness != "" {
		params.Set("freshness", input.Freshness)
	}

	data, err := h.client.doSearchRequest(ctx, "/res/v1/web/search", params)
	if err != nil {
		return nil, nil, fmt.Errorf("web search failed: %w", err)
	}

	return nil, formatWebResults(data), nil
}

type ImageSearchInput struct {
	Query      string `json:"query" jsonschema:"required,description=Search query"`
	Count      int    `json:"count,omitempty" jsonschema:"description=Number of results (1-200, default 20)"`
	Safesearch string `json:"safesearch,omitempty" jsonschema:"description=Filter: off or strict"`
}

func (h *ToolHandlers) handleImageSearch(ctx context.Context, req *mcp.CallToolRequest, input ImageSearchInput) (*mcp.CallToolResult, any, error) {
	params := url.Values{}
	params.Set("q", input.Query)
	if input.Count > 0 {
		params.Set("count", fmt.Sprintf("%d", input.Count))
	}
	if input.Safesearch != "" {
		params.Set("safesearch", input.Safesearch)
	}

	data, err := h.client.doSearchRequest(ctx, "/res/v1/images/search", params)
	if err != nil {
		return nil, nil, fmt.Errorf("image search failed: %w", err)
	}

	return nil, formatImageResults(data), nil
}

type VideoSearchInput struct {
	Query      string `json:"query" jsonschema:"required,description=Search query"`
	Count      int    `json:"count,omitempty" jsonschema:"description=Number of results (1-50, default 20)"`
	Offset     int    `json:"offset,omitempty" jsonschema:"description=Pagination offset (0-9)"`
	Safesearch string `json:"safesearch,omitempty" jsonschema:"description=Filter: off, moderate, or strict"`
	Freshness  string `json:"freshness,omitempty" jsonschema:"description=Age filter: pd, pw, pm, py"`
}

func (h *ToolHandlers) handleVideoSearch(ctx context.Context, req *mcp.CallToolRequest, input VideoSearchInput) (*mcp.CallToolResult, any, error) {
	params := url.Values{}
	params.Set("q", input.Query)
	if input.Count > 0 {
		params.Set("count", fmt.Sprintf("%d", input.Count))
	}
	if input.Offset > 0 {
		params.Set("offset", fmt.Sprintf("%d", input.Offset))
	}
	if input.Safesearch != "" {
		params.Set("safesearch", input.Safesearch)
	}
	if input.Freshness != "" {
		params.Set("freshness", input.Freshness)
	}

	data, err := h.client.doSearchRequest(ctx, "/res/v1/videos/search", params)
	if err != nil {
		return nil, nil, fmt.Errorf("video search failed: %w", err)
	}

	return nil, formatVideoResults(data), nil
}

type NewsSearchInput struct {
	Query      string `json:"query" jsonschema:"required,description=Search query"`
	Count      int    `json:"count,omitempty" jsonschema:"description=Number of results (1-50, default 20)"`
	Offset     int    `json:"offset,omitempty" jsonschema:"description=Pagination offset (0-9)"`
	Safesearch string `json:"safesearch,omitempty" jsonschema:"description=Filter: off, moderate, or strict"`
	Freshness  string `json:"freshness,omitempty" jsonschema:"description=Age filter: pd, pw, pm, py"`
}

func (h *ToolHandlers) handleNewsSearch(ctx context.Context, req *mcp.CallToolRequest, input NewsSearchInput) (*mcp.CallToolResult, any, error) {
	params := url.Values{}
	params.Set("q", input.Query)
	if input.Count > 0 {
		params.Set("count", fmt.Sprintf("%d", input.Count))
	}
	if input.Offset > 0 {
		params.Set("offset", fmt.Sprintf("%d", input.Offset))
	}
	if input.Safesearch != "" {
		params.Set("safesearch", input.Safesearch)
	}
	if input.Freshness != "" {
		params.Set("freshness", input.Freshness)
	}

	data, err := h.client.doSearchRequest(ctx, "/res/v1/news/search", params)
	if err != nil {
		return nil, nil, fmt.Errorf("news search failed: %w", err)
	}

	return nil, formatNewsResults(data), nil
}

type LocalSearchInput struct {
	Query      string `json:"query" jsonschema:"required,description=Search query"`
	Count      int    `json:"count,omitempty" jsonschema:"description=Number of results"`
	Safesearch string `json:"safesearch,omitempty" jsonschema:"description=Filter: off, moderate, or strict"`
	Freshness  string `json:"freshness,omitempty" jsonschema:"description=Age filter: pd, pw, pm, py"`
}

func (h *ToolHandlers) handleLocalSearch(ctx context.Context, req *mcp.CallToolRequest, input LocalSearchInput) (*mcp.CallToolResult, any, error) {
	params := url.Values{}
	params.Set("q", input.Query)
	params.Set("result_filter", "web,locations")
	if input.Count > 0 {
		params.Set("count", fmt.Sprintf("%d", input.Count))
	}
	if input.Safesearch != "" {
		params.Set("safesearch", input.Safesearch)
	}
	if input.Freshness != "" {
		params.Set("freshness", input.Freshness)
	}

	webData, err := h.client.doSearchRequest(ctx, "/res/v1/web/search", params)
	if err != nil {
		return nil, nil, fmt.Errorf("local search failed: %w", err)
	}

	var webResp struct {
		Locations *struct {
			Results []struct {
				ID         string `json:"id"`
				Title      string `json:"title"`
				PriceRange string `json:"price_range"`
				Contact    struct {
					Telephone string `json:"telephone"`
				} `json:"contact"`
				Rating struct {
					RatingValue float64 `json:"ratingValue"`
					ReviewCount int     `json:"reviewCount"`
				} `json:"rating"`
				PostalAddress struct {
					DisplayAddress string `json:"displayAddress"`
				} `json:"postal_address"`
			} `json:"results"`
		} `json:"locations"`
		Web *struct {
			Results []struct {
				URL         string `json:"url"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}

	if err := json.Unmarshal(webData, &webResp); err != nil {
		return nil, nil, fmt.Errorf("parsing local search response: %w", err)
	}

	if webResp.Locations == nil || len(webResp.Locations.Results) == 0 {
		if webResp.Web != nil && len(webResp.Web.Results) > 0 {
			return nil, "No local results found. Falling back to web results:\n\n" + formatWebResults(webData), nil
		}
		return nil, "No local results found.", nil
	}

	locationIDs := make([]string, 0, 20)
	for i, loc := range webResp.Locations.Results {
		if i >= 20 {
			break
		}
		locationIDs = append(locationIDs, loc.ID)
	}

	locationsJSON, err := json.Marshal(webResp.Locations.Results)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling locations: %w", err)
	}

	var descData json.RawMessage
	descParams := url.Values{}
	for _, id := range locationIDs {
		descParams.Add("ids", id)
	}
	descData, err = h.client.doRequest(ctx, "/res/v1/local/descriptions", descParams)
	if err != nil {
		descData = nil
	}

	return nil, formatLocalResults(json.RawMessage(locationsJSON), descData), nil
}

type AnswersInput struct {
	Query      string `json:"query" jsonschema:"required,description=Question to get an AI-generated answer for"`
	Safesearch string `json:"safesearch,omitempty" jsonschema:"description=Filter: off, moderate, or strict"`
}

func (h *ToolHandlers) handleAnswers(ctx context.Context, req *mcp.CallToolRequest, input AnswersInput) (*mcp.CallToolResult, any, error) {
	body := map[string]interface{}{
		"messages":         []map[string]string{{"role": "user", "content": input.Query}},
		"model":            "brave",
		"stream":           false,
		"enable_citations": true,
		"enable_entities":  true,
	}
	if h.client.country != "" {
		body["country"] = h.client.country
	}
	if h.client.lang != "" {
		body["language"] = h.client.lang
	}
	if input.Safesearch != "" {
		body["safesearch"] = input.Safesearch
	}

	data, err := h.client.doPostRequest(ctx, "/res/v1/chat/completions", body)
	if err != nil {
		return nil, nil, fmt.Errorf("answers search failed: %w", err)
	}

	return nil, formatAnswersResults(data), nil
}

type LLMContextInput struct {
	Query     string `json:"query" jsonschema:"required,description=Search query"`
	Count     int    `json:"count,omitempty" jsonschema:"description=Number of results (1-50)"`
	Freshness string `json:"freshness,omitempty" jsonschema:"description=Age filter: pd, pw, pm, py"`
}

func (h *ToolHandlers) handleLLMContext(ctx context.Context, req *mcp.CallToolRequest, input LLMContextInput) (*mcp.CallToolResult, any, error) {
	params := url.Values{}
	params.Set("q", input.Query)
	if input.Count > 0 {
		params.Set("count", fmt.Sprintf("%d", input.Count))
	}
	if input.Freshness != "" {
		params.Set("freshness", input.Freshness)
	}

	data, err := h.client.doSearchRequest(ctx, "/res/v1/llm/context", params)
	if err != nil {
		return nil, nil, fmt.Errorf("LLM context search failed: %w", err)
	}

	return nil, formatLLMContextResults(data), nil
}

type PlaceSearchInput struct {
	Query      string   `json:"query,omitempty" jsonschema:"description=Search query"`
	Latitude   *float64 `json:"latitude,omitempty" jsonschema:"description=Latitude (-90 to 90)"`
	Longitude  *float64 `json:"longitude,omitempty" jsonschema:"description=Longitude (-180 to 180)"`
	Location   string   `json:"location,omitempty" jsonschema:"description=Location string (e.g. 'san francisco ca united states')"`
	Radius     *float64 `json:"radius,omitempty" jsonschema:"description=Search radius bias in meters"`
	Count      int      `json:"count,omitempty" jsonschema:"description=Number of results (1-50, default 20)"`
	Safesearch string   `json:"safesearch,omitempty" jsonschema:"description=Filter: off, moderate, or strict"`
}

func (h *ToolHandlers) handlePlaceSearch(ctx context.Context, req *mcp.CallToolRequest, input PlaceSearchInput) (*mcp.CallToolResult, any, error) {
	params := url.Values{}
	if input.Query != "" {
		params.Set("q", input.Query)
	}
	if input.Latitude != nil {
		params.Set("latitude", fmt.Sprintf("%f", *input.Latitude))
	}
	if input.Longitude != nil {
		params.Set("longitude", fmt.Sprintf("%f", *input.Longitude))
	}
	if input.Location != "" {
		params.Set("location", input.Location)
	}
	if input.Radius != nil && *input.Radius > 0 {
		params.Set("radius", fmt.Sprintf("%f", *input.Radius))
	}
	if input.Count > 0 {
		params.Set("count", fmt.Sprintf("%d", input.Count))
	}
	if input.Safesearch != "" {
		params.Set("safesearch", input.Safesearch)
	}

	data, err := h.client.doSearchRequest(ctx, "/res/v1/local/place_search", params)
	if err != nil {
		return nil, nil, fmt.Errorf("place search failed: %w", err)
	}

	return nil, formatPlaceResults(data), nil
}

var allToolNames = []string{
	"brave_web_search",
	"brave_image_search",
	"brave_video_search",
	"brave_news_search",
	"brave_local_search",
	"brave_answers",
	"brave_llm_context",
	"brave_place_search",
}

func registerTools(server *mcp.Server, handlers *ToolHandlers, enabled map[string]bool) {
	if enabled == nil || enabled["brave_web_search"] {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "brave_web_search",
			Description: "Search the web using Brave Search. Returns web pages, FAQ, discussions, news, and video results.",
		}, handlers.handleWebSearch)
	}
	if enabled == nil || enabled["brave_image_search"] {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "brave_image_search",
			Description: "Search for images using Brave Search.",
		}, handlers.handleImageSearch)
	}
	if enabled == nil || enabled["brave_video_search"] {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "brave_video_search",
			Description: "Search for videos using Brave Search. Returns video results with duration and metadata.",
		}, handlers.handleVideoSearch)
	}
	if enabled == nil || enabled["brave_news_search"] {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "brave_news_search",
			Description: "Search for news articles using Brave Search. Returns recent news with source and age.",
		}, handlers.handleNewsSearch)
	}
	if enabled == nil || enabled["brave_local_search"] {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "brave_local_search",
			Description: "Search for local businesses and places. Falls back to web results if no locations found.",
		}, handlers.handleLocalSearch)
	}
	if enabled == nil || enabled["brave_answers"] {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "brave_answers",
			Description: "Get an AI-generated answer to a question using Brave's chat completions API. Returns a direct answer with citations.",
		}, handlers.handleAnswers)
	}
	if enabled == nil || enabled["brave_llm_context"] {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "brave_llm_context",
			Description: "Get pre-extracted web content optimized for RAG and AI reasoning. Returns actual page text, not just links.",
		}, handlers.handleLLMContext)
	}
	if enabled == nil || enabled["brave_place_search"] {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "brave_place_search",
			Description: "Search for geographic places and POIs. Supports coordinate-based and named-location queries.",
		}, handlers.handlePlaceSearch)
	}
}
