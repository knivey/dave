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

	u := c.baseURL + endpoint + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
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

func (c *braveClient) doSearchRequest(ctx context.Context, endpoint string, params url.Values) (json.RawMessage, error) {
	if params == nil {
		params = url.Values{}
	}
	if c.country != "" {
		params.Set("country", c.country)
	}
	if c.lang != "" {
		params.Set("search_lang", c.lang)
	}
	return c.doRequest(ctx, endpoint, params)
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
	hasContent := false

	for i, r := range resp.Web.Results {
		if hasContent {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s", i+1, r.Title, r.URL, r.Description)
		hasContent = true
	}

	if resp.FAQ != nil {
		for _, f := range resp.FAQ.Results {
			if hasContent {
				b.WriteString("\n\n---\n")
			}
			fmt.Fprintf(&b, "FAQ: %s\n%s\n%s", f.Question, f.Answer, f.URL)
			hasContent = true
		}
	}

	if resp.News != nil {
		for _, n := range resp.News.Results {
			if hasContent {
				b.WriteString("\n\n---\n")
			}
			fmt.Fprintf(&b, "News: %s (%s)\n%s\n   %s", n.Title, n.Age, n.Description, n.URL)
			hasContent = true
		}
	}

	if resp.Videos != nil {
		for _, v := range resp.Videos.Results {
			if hasContent {
				b.WriteString("\n\n---\n")
			}
			fmt.Fprintf(&b, "Video: %s [%s] (%s)\n%s\n   %s", v.Title, v.Video.Duration, v.Age, v.Description, v.URL)
			hasContent = true
		}
	}

	return b.String()
}

func formatImageResults(data json.RawMessage) string {
	var resp struct {
		Results []struct {
			URL       string `json:"url"`
			Title     string `json:"title"`
			Source    string `json:"source"`
			Thumbnail struct {
				Src string `json:"src"`
			} `json:"thumbnail"`
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
		if r.Thumbnail.Src != "" {
			fmt.Fprintf(&b, "\n   Thumbnail: %s", r.Thumbnail.Src)
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
			MetaURL     struct {
				Hostname string `json:"hostname"`
			} `json:"meta_url"`
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
		source := r.MetaURL.Hostname
		if source != "" && r.Age != "" {
			fmt.Fprintf(&b, "%d. %s (%s, %s)\n   %s\n   %s", i+1, r.Title, source, r.Age, r.Description, r.URL)
		} else if r.Age != "" {
			fmt.Fprintf(&b, "%d. %s (%s)\n   %s\n   %s", i+1, r.Title, r.Age, r.Description, r.URL)
		} else {
			fmt.Fprintf(&b, "%d. %s\n   %s\n   %s", i+1, r.Title, r.Description, r.URL)
		}
	}
	return b.String()
}

func formatLocalResults(poisData json.RawMessage, descData json.RawMessage) string {
	var pois struct {
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
		if p.Contact.Telephone != "" {
			fmt.Fprintf(&b, "\n   Phone: %s", p.Contact.Telephone)
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
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		} `json:"summary"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}

	var b strings.Builder
	for _, part := range resp.Summary {
		if part.Type == "token" {
			var s string
			if err := json.Unmarshal(part.Data, &s); err == nil {
				b.WriteString(s)
			}
		} else if part.Type == "inline_reference" {
			var ref struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(part.Data, &ref); err == nil && ref.URL != "" {
				fmt.Fprintf(&b, " (%s)", ref.URL)
			}
		}
	}
	return b.String()
}

func formatLLMContextResults(data json.RawMessage) string {
	var resp struct {
		Grounding struct {
			Generic []struct {
				URL      string   `json:"url"`
				Title    string   `json:"title"`
				Snippets []string `json:"snippets"`
			} `json:"generic"`
		} `json:"grounding"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}

	var b strings.Builder
	hasContent := false
	for _, r := range resp.Grounding.Generic {
		if hasContent {
			b.WriteString("\n\n---\n")
		}
		fmt.Fprintf(&b, "## %s\n%s", r.Title, r.URL)
		for _, s := range r.Snippets {
			fmt.Fprintf(&b, "\n\n%s", s)
		}
		hasContent = true
	}

	if !hasContent {
		return string(data)
	}
	return b.String()
}

func formatPlaceResults(data json.RawMessage) string {
	var resp struct {
		Results []struct {
			Name    string `json:"title"`
			Contact struct {
				Telephone string `json:"telephone"`
			} `json:"contact"`
			Rating struct {
				RatingValue float64 `json:"ratingValue"`
				ReviewCount int     `json:"reviewCount"`
			} `json:"rating"`
			PostalAddress struct {
				DisplayAddress string `json:"displayAddress"`
			} `json:"postal_address"`
			Distance struct {
				Value float64 `json:"value"`
			} `json:"distance"`
			IconCategory string   `json:"icon_category"`
			Categories   []string `json:"categories"`
		} `json:"results"`
		Cities []struct {
			Name string `json:"name"`
		} `json:"cities"`
		Addresses []struct {
			Name string `json:"name"`
		} `json:"addresses"`
	}

	if err := json.Unmarshal(data, &resp); err != nil {
		return string(data)
	}

	var b strings.Builder
	hasContent := false

	for i, r := range resp.Results {
		if hasContent {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%d. %s", i+1, r.Name)
		if r.Rating.RatingValue > 0 {
			fmt.Fprintf(&b, " (%.1f/5, %d reviews)", r.Rating.RatingValue, r.Rating.ReviewCount)
		}
		if r.IconCategory != "" {
			fmt.Fprintf(&b, " [%s]", r.IconCategory)
		} else if len(r.Categories) > 0 {
			fmt.Fprintf(&b, " [%s]", r.Categories[0])
		}
		if r.Distance.Value > 0 {
			fmt.Fprintf(&b, " (%.0fm away)", r.Distance.Value)
		}
		if r.PostalAddress.DisplayAddress != "" {
			fmt.Fprintf(&b, "\n   %s", r.PostalAddress.DisplayAddress)
		}
		if r.Contact.Telephone != "" {
			fmt.Fprintf(&b, "\n   Phone: %s", r.Contact.Telephone)
		}
		hasContent = true
	}

	for _, c := range resp.Cities {
		if hasContent {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "City: %s", c.Name)
		hasContent = true
	}

	for _, a := range resp.Addresses {
		if hasContent {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "Address: %s", a.Name)
		hasContent = true
	}

	if !hasContent {
		return string(data)
	}
	return b.String()
}
