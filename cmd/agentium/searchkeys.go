package main

import (
	"os"

	"github.com/tegarthegreat/agentium/internal/config"
)

// searchKeyEnv maps the web search services `agentium login` accepts to
// the environment variable the web_search tool reads.
var searchKeyEnv = map[string]string{
	"brave":  "BRAVE_API_KEY",
	"tavily": "TAVILY_API_KEY",
	"exa":    "EXA_API_KEY",
	"serper": "SERPER_API_KEY",
}

// searchAuthID is where a search service's key is stored in auth.json or
// the keychain (apart from model providers).
func searchAuthID(id string) string { return "search-" + id }

// exportSearchKeys makes stored search keys visible to web_search, unless
// the environment already sets them. Commands the agent runs do not see
// them: credential-like variables are removed from their environment.
func exportSearchKeys(auth config.Auth) {
	for id, env := range searchKeyEnv {
		if os.Getenv(env) != "" {
			continue
		}
		if c, ok := auth[searchAuthID(id)]; ok {
			if k := c.Secret(searchAuthID(id)); k != "" {
				os.Setenv(env, k)
			}
		}
	}
}
