package connect

// Cursor has no settings file for the key or the address, so connect writes nothing and stores nothing. It prints
// the steps of the console's Cursor tab (web/src/lib/tool-setup.ts); the texts are the same.

// CursorGuide is what `ambit connect cursor` prints.
type CursorGuide struct {
	Values  [][2]string // label and text: the three fields to fill in Cursor
	Steps   []string
	Warning string // the address is not public HTTPS; empty when it is
	KeyHint string // how to copy the stored key; empty when no key is stored for this address
}

func ConnectCursor(url, model string, keyStored bool) CursorGuide {
	if model == "" {
		model = "<model-id>"
	}
	g := CursorGuide{
		Values: [][2]string{{"OpenAI API Key", "your Ambit key"}, {"Override OpenAI Base URL", url + "/v1"}, {"Model name", model}},
		Steps: []string{
			"In Cursor, open Settings, then Models.",
			"Under API Keys, turn on \"OpenAI API Key\" and paste the key. Turn on \"Override OpenAI Base URL\" and paste the address.",
			"Add the model name with \"Add model\". Cursor has no settings file for this.",
		},
	}
	if !IsPublicHTTPS(url) {
		g.Warning = "Cursor sends its requests from Cursor's servers, not from your computer. The LLM address " + url + " is not a public HTTPS address, so Cursor cannot reach it. The address must be public HTTPS."
	}
	if keyStored {
		g.KeyHint = "Copy the key with: ambit key print --url " + url + " --reveal"
	}
	return g
}

// DisconnectCursor is the reverse of the steps above.
func DisconnectCursor() []string {
	return []string{
		"In Cursor, open Settings, then Models.",
		"Turn off \"Override OpenAI Base URL\" and clear the address.",
		"Turn off \"OpenAI API Key\" and remove the key, and remove the model name you added.",
	}
}
