package anthropic

import "github.com/staysup/agentbus/body"

// Split cuts a Messages request into pieces: each system block (or the
// system string as one piece), each tool definition, each message. Everything
// else stays in Top. A system value that is neither a string nor an array, or
// tools or messages that are not arrays, stay in Top unchanged.
func (Adapter) Split(data []byte) (body.Request, error) {
	fields, err := body.Fields(data)
	if err != nil {
		return body.Request{}, err
	}
	var req body.Request
	var top []body.Field
	for _, f := range fields {
		switch f.Key {
		case "system":
			if body.IsString(f.Value) {
				req.Pieces = append(req.Pieces, body.Piece{Kind: body.System, Label: "text", JSON: f.Value})
				continue
			}
			if els, ok := body.Elements(f.Value); ok {
				for _, e := range els {
					req.Pieces = append(req.Pieces, body.Piece{Kind: body.System, Label: body.Label(e, "type"), JSON: e})
				}
				continue
			}
		case "tools":
			if els, ok := body.Elements(f.Value); ok {
				for _, e := range els {
					req.Pieces = append(req.Pieces, body.Piece{Kind: body.Tool, Label: body.Label(e, "name", "type"), JSON: e})
				}
				continue
			}
		case "messages":
			if els, ok := body.Elements(f.Value); ok {
				for _, e := range els {
					req.Pieces = append(req.Pieces, body.Piece{Kind: body.Message, Label: body.Label(e, "role"), JSON: e})
				}
				continue
			}
		}
		top = append(top, f)
	}
	req.Top = body.Object(top)
	return req, nil
}
