package stylus

import (
	"fmt"
	"strings"
	"unicode"
)

type eventSignature struct {
	Name   string
	Params []eventParameter
}

type eventParameter struct {
	Type    string
	Name    string
	Indexed bool
}

func parseEventSignature(signature string) (eventSignature, error) {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return eventSignature{}, fmt.Errorf("empty signature")
	}

	if strings.HasPrefix(signature, "event") {
		afterKeyword := signature[len("event"):]
		if afterKeyword == "" {
			return eventSignature{}, fmt.Errorf("event signature missing identifier and parameters")
		}

		trimmed := strings.TrimLeftFunc(afterKeyword, unicode.IsSpace)
		if len(trimmed) != len(afterKeyword) {
			signature = strings.TrimSpace(trimmed)
		}
	}

	if strings.HasSuffix(signature, ";") {
		signature = strings.TrimSpace(signature[:len(signature)-1])
	}

	if signature == "" {
		return eventSignature{}, fmt.Errorf("event signature missing identifier and parameters")
	}

	open := strings.Index(signature, "(")
	close := strings.LastIndex(signature, ")")
	if open == -1 || close == -1 || close < open {
		return eventSignature{}, fmt.Errorf("invalid event signature format: %s", signature)
	}

	name := strings.TrimSpace(signature[:open])
	if name == "" {
		return eventSignature{}, fmt.Errorf("event name missing in signature: %s", signature)
	}

	paramsRaw := signature[open+1 : close]
	paramStrings, err := splitEventParameters(paramsRaw)
	if err != nil {
		return eventSignature{}, err
	}

	params := make([]eventParameter, 0, len(paramStrings))
	for idx, paramString := range paramStrings {
		paramString = strings.TrimSpace(paramString)
		if paramString == "" {
			continue
		}

		param, err := parseEventParameter(paramString)
		if err != nil {
			return eventSignature{}, fmt.Errorf("parse param %d: %w", idx, err)
		}
		params = append(params, param)
	}

	return eventSignature{Name: name, Params: params}, nil
}

func splitEventParameters(params string) ([]string, error) {
	if strings.TrimSpace(params) == "" {
		return nil, nil
	}

	result := []string{}
	depth := 0
	last := 0
	for i, r := range params {
		switch r {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return nil, fmt.Errorf("unbalanced parentheses in parameter list: %s", params)
			}
			depth--
		case ',':
			if depth == 0 {
				segment := strings.TrimSpace(params[last:i])
				result = append(result, segment)
				last = i + 1
			}
		}
	}

	if depth != 0 {
		return nil, fmt.Errorf("unbalanced parentheses in parameter list: %s", params)
	}

	finalSegment := strings.TrimSpace(params[last:])
	if finalSegment != "" {
		result = append(result, finalSegment)
	}

	return result, nil
}

func parseEventParameter(param string) (eventParameter, error) {
	fields := strings.Fields(param)
	if len(fields) == 0 {
		return eventParameter{}, fmt.Errorf("empty parameter")
	}

	result := eventParameter{Type: fields[0]}
	idx := 1

	if idx < len(fields) && fields[idx] == "payable" {
		result.Type += " payable"
		idx++
	}

	for idx < len(fields) && fields[idx] == "indexed" {
		result.Indexed = true
		idx++
	}

	if idx < len(fields) {
		result.Name = fields[idx]
		idx++
	}

	if idx < len(fields) {
		return eventParameter{}, fmt.Errorf("unexpected tokens in parameter: %s", param)
	}

	return result, nil
}
