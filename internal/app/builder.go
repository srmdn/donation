package app

import (
	"net/url"
	"os"
	"strings"
)

func DefaultBuilder() Builder {
	builder := Builder{
		Name:       envOr("BUILDER_NAME", "Said Ramadhan"),
		Handle:     envOr("BUILDER_HANDLE", "srmdn"),
		Bio:        envOr("BUILDER_BIO", "I build small, durable tools for publishing, learning, and self-hosted workflows."),
		AvatarURL:  strings.TrimSpace(os.Getenv("BUILDER_AVATAR_URL")),
		WebsiteURL: strings.TrimSpace(os.Getenv("BUILDER_WEBSITE_URL")),
		GitHubURL:  envOr("BUILDER_GITHUB_URL", "https://github.com/srmdn"),
		GitLabURL:  strings.TrimSpace(os.Getenv("BUILDER_GITLAB_URL")),
	}

	if builder.AvatarURL == "" {
		builder.AvatarURL = githubAvatarURL(builder.GitHubURL)
	}

	return builder
}

func envOr(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func githubAvatarURL(profileURL string) string {
	profileURL = strings.TrimSpace(profileURL)
	if profileURL == "" {
		return ""
	}

	parsed, err := url.Parse(profileURL)
	if err != nil {
		return ""
	}
	if !strings.EqualFold(parsed.Hostname(), "github.com") {
		return ""
	}

	username := strings.Trim(strings.TrimSpace(parsed.Path), "/")
	if username == "" || strings.Contains(username, "/") {
		return ""
	}

	return "https://github.com/" + username + ".png"
}
