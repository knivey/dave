package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type braveClient struct {
	apiKey     string
	baseURL    string
	timeout    time.Duration
	country    string
	lang       string
	httpClient *http.Client
}

func newBraveClient(apiKey, baseURL string, timeout time.Duration, country, lang string) *braveClient {
	return &braveClient{
		apiKey:  apiKey,
		baseURL: baseURL,
		timeout: timeout,
		country: country,
		lang:    lang,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *braveClient) doRequest(ctx context.Context, endpoint string, params url.Values) (json.RawMessage, error) {
	if params == nil {
		params = url.Values{}
	}
	if c.country != "" {
		params.Set("country", c.country)
	}
	if c.lang != "" {
		params.Set("search_lang", c.lang)
	}

	u := c.baseURL + endpoint + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

func formatWebResults(data json.RawMessage) string {
	var resp struct {
		Web struct {
			Results []struct {
				URL         string `json:"url"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
		FAQ *struct {
			Results []struct {
				Question string `json:"question"`
				Answer   string `json:"answer"`
				Title    string `json:"title"`
				URL      string `json:"url"`
			} `json:"results"`
		} `json:"faq"`
		News *struct {
			Results []struct {
				URL         string `json:"url"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Age         string `json:"age"`
				Source      string `json:"source"`
			} `json:"results"`
		} `json:"news"`
		Videos *struct {
			Results []struct {
				URL         string `json:"url"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Age         string `json:"age"`
				Video       struct {
					Duration string `json:"duration"`
				} `json:"video"`
			} `json:"results"`
		} `json:"videos"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}

	var b strings.Builder

	for i, r := range resp.Web.Results {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s", i+1, r.Title, r.URL, r.Description)
	}

	if resp.FAQ != nil {
		for _, f := range resp.FAQ.Results {
			b.WriteString("\n\n---\n")
			fmt.Fprintf(&b, "FAQ: %s\n%s\n%s", f.Question, f.Answer, f.URL)
		}
	}

	if resp.News != nil {
		for _, n := range resp.News.Results {
			b.WriteString("\n\n---\n")
			fmt.Fprintf(&b, "News: %s (%s)\n%s\n   %s", n.Title, n.Age, n.Description, n.URL)
		}
	}

	if resp.Videos != nil {
		for _, v := range resp.Videos.Results {
			b.WriteString("\n\n---\n")
			fmt.Fprintf(&b, "Video: %s [%s] (%s)\n%s\n   %s", v.Title, v.Video.Duration, v.Age, v.Description, v.URL)
		}
	}

	return b.String()
}

func formatImageResults(data json.RawMessage) string {
	var resp struct {
		Results []struct {
			URL         string `json:"url"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Source      string `json:"source"`
		} `json:"results"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}

	var b strings.Builder
	for i, r := range resp.Results {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   Source: %s", i+1, r.Title, r.URL, r.Source)
		if r.Description != "" {
			fmt.Fprintf(&b, "\n   %s", r.Description)
		}
	}
	return b.String()
}

func formatVideoResults(data json.RawMessage) string {
	var resp struct {
		Results []struct {
			URL         string `json:"url"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Age         string `json:"age"`
			Video       struct {
				Duration string `json:"duration"`
				Views    int    `json:"views"`
				Creator  string `json:"creator"`
			} `json:"video"`
		} `json:"results"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}

	var b strings.Builder
	for i, r := range resp.Results {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s [%s]", i+1, r.Title, r.Video.Duration)
		if r.Video.Creator != "" {
			fmt.Fprintf(&b, " by %s", r.Video.Creator)
		}
		if r.Age != "" {
			fmt.Fprintf(&b, " (%s)", r.Age)
		}
		fmt.Fprintf(&b, "\n   %s", r.URL)
		if r.Description != "" {
			fmt.Fprintf(&b, "\n   %s", r.Description)
		}
	}
	return b.String()
}

func formatNewsResults(data json.RawMessage) string {
	var resp struct {
		Results []struct {
			URL         string `json:"url"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Age         string `json:"age"`
			Source      string `json:"source"`
		} `json:"results"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}

	var b strings.Builder
	for i, r := range resp.Results {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s (%s, %s)\n   %s\n   %s", i+1, r.Title, r.Source, r.Age, r.Description, r.URL)
	}
	return b.String()
}

func formatLocalResults(poisData json.RawMessage, descData json.RawMessage) string {
	var pois struct {
		Results []struct {
			ID         string `json:"id"`
			Title      string `json:"title"`
			PriceRange string `json:"price_range"`
			Phone      string `json:"phone"`
			Rating     struct {
				RatingValue float64 `json:"ratingValue"`
				ReviewCount int     `json:"reviewCount"`
			} `json:"rating"`
			PostalAddress struct {
				DisplayAddress string `json:"displayAddress"`
			} `json:"postal_address"`
		} `json:"results"`
	}

	if err := json.Unmarshal(poisData, &pois); err != nil {
		return string(poisData)
	}

	descriptions := map[string]string{}
	if descData != nil {
		var desc struct {
			Results []struct {
				ID          string `json:"id"`
				Description string `json:"description"`
			} `json:"results"`
		}
		if err := json.Unmarshal(descData, &desc); err == nil {
			for _, d := range desc.Results {
				descriptions[d.ID] = d.Description
			}
		}
	}

	var b strings.Builder
	for i, p := range pois.Results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%d. %s", i+1, p.Title)
		if p.Rating.RatingValue > 0 {
			fmt.Fprintf(&b, " (%.1f/5, %d reviews)", p.Rating.RatingValue, p.Rating.ReviewCount)
		}
		if p.PriceRange != "" {
			fmt.Fprintf(&b, " %s", p.PriceRange)
		}
		if p.PostalAddress.DisplayAddress != "" {
			fmt.Fprintf(&b, "\n   %s", p.PostalAddress.DisplayAddress)
		}
		if p.Phone != "" {
			fmt.Fprintf(&b, "\n   Phone: %s", p.Phone)
		}
		if d, ok := descriptions[p.ID]; ok && d != "" {
			fmt.Fprintf(&b, "\n   %s", d)
		}
	}
	return b.String()
}

func formatSummarizerResults(data json.RawMessage) string {
	var resp struct {
		Status  string `json:"status"`
		Summary []struct {
			Type string `json:"type"`
			Data string `json:"data"`
		} `json:"summary"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}

	var b strings.Builder
	for _, part := range resp.Summary {
		if part.Type == "token" {
			b.WriteString(part.Data)
		} else if part.Type == "inline_reference" {
			fmt.Fprintf(&b, " (%s)", part.Data)
		}
	}
	return b.String()
}
