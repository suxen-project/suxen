package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"go.yaml.in/yaml/v3"
)

// decodeRawResources keeps YAML's scalar semantics while retaining the exact
// spelling of numeric values. Decode the headers separately so yaml/v3 cannot
// round or reject a large spec number before it reaches yamlJSONValue.
func decodeRawResources(nodes []*yaml.Node) ([]rawResource, error) {
	resources := make([]rawResource, len(nodes))
	budget := yamlSpecBudget{visits: 200000, bytes: MaxDocumentSize}
	for i, node := range nodes {
		var header struct {
			Kind string `yaml:"kind"`
			Name string `yaml:"name"`
		}
		if err := node.Decode(&header); err != nil {
			return nil, fmt.Errorf("resource %d: %w", i+1, err)
		}
		resources[i] = rawResource{Kind: header.Kind, Name: header.Name}
		if specNode := mappingValue(node, "spec"); specNode != nil && specNode.Tag != "!!null" {
			value, err := yamlJSONValue(specNode, make(map[*yaml.Node]bool), &budget)
			if err != nil {
				return nil, fmt.Errorf("resource %d spec: %w", i+1, err)
			}
			spec, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("resource %d spec must be a mapping", i+1)
			}
			resources[i].Spec = spec
		}
	}
	return resources, nil
}

type yamlSpecBudget struct {
	visits int
	bytes  int
}

func yamlJSONValue(node *yaml.Node, active map[*yaml.Node]bool, budget *yamlSpecBudget) (any, error) {
	budget.visits--
	if budget.visits < 0 {
		return nil, errors.New("YAML spec alias expansion exceeds limit")
	}
	if active[node] {
		return nil, errors.New("cyclic YAML alias")
	}
	active[node] = true
	defer delete(active, node)
	switch node.Kind {
	case yaml.AliasNode:
		return yamlJSONValue(node.Alias, active, budget)
	case yaml.MappingNode:
		result := make(map[string]any, len(node.Content)/2)
		// YAML merges supply defaults; explicit keys override them regardless of order.
		seenMerge := false
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Tag != "!!merge" {
				continue
			}
			if seenMerge {
				return nil, errors.New("duplicate YAML spec key \"<<\"")
			}
			seenMerge = true
			merge := node.Content[i+1]
			sources := []*yaml.Node{merge}
			if merge.Kind == yaml.SequenceNode {
				sources = merge.Content
			}
			for _, source := range sources {
				value, err := yamlJSONValue(source, active, budget)
				if err != nil {
					return nil, err
				}
				mapping, ok := value.(map[string]any)
				if !ok {
					return nil, errors.New("YAML merge source must be a mapping")
				}
				for key, item := range mapping {
					if _, exists := result[key]; !exists {
						result[key] = item
					}
				}
			}
		}
		explicit := make(map[string]bool, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			if keyNode.Tag == "!!merge" {
				continue
			}
			if keyNode.Kind != yaml.ScalarNode || keyNode.Tag != "!!str" {
				return nil, fmt.Errorf("YAML spec mapping key %q must be a string", keyNode.Value)
			}
			budget.bytes -= len(keyNode.Value)
			if budget.bytes < 0 {
				return nil, errors.New("YAML spec alias expansion exceeds byte limit")
			}
			var key string
			if err := keyNode.Decode(&key); err != nil {
				return nil, err
			}
			if explicit[key] {
				return nil, fmt.Errorf("duplicate YAML spec key %q", key)
			}
			explicit[key] = true
			value, err := yamlJSONValue(node.Content[i+1], active, budget)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, len(node.Content))
		for i, child := range node.Content {
			value, err := yamlJSONValue(child, active, budget)
			if err != nil {
				return nil, err
			}
			result[i] = value
		}
		return result, nil
	case yaml.ScalarNode:
		budget.bytes -= len(node.Value)
		if budget.bytes < 0 {
			return nil, errors.New("YAML spec alias expansion exceeds byte limit")
		}
		// yaml/v3 resolves otherwise valid numeric scalars with an exponent
		// outside float64's range as strings. Keep their numeric meaning and
		// exact value; quoted strings retain their string meaning.
		if node.Tag == "!!str" && node.Style == 0 && json.Valid([]byte(node.Value)) {
			var number json.Number
			if err := decodeStrictJSON([]byte(node.Value), &number); err == nil {
				return number, nil
			}
		}
		switch node.Tag {
		case "!!int":
			integer, ok := new(big.Int).SetString(strings.ReplaceAll(node.Value, "_", ""), 0)
			if !ok {
				return nil, fmt.Errorf("invalid YAML integer %q", node.Value)
			}
			return json.Number(integer.String()), nil
		case "!!float":
			number := strings.ReplaceAll(node.Value, "_", "")
			if strings.HasPrefix(number, "+") {
				number = number[1:]
			}
			unsigned := strings.TrimPrefix(number, "-")
			if strings.HasPrefix(unsigned, "0x") || strings.HasPrefix(unsigned, "0X") || strings.HasPrefix(unsigned, "0o") || strings.HasPrefix(unsigned, "0O") || strings.HasPrefix(unsigned, "0b") || strings.HasPrefix(unsigned, "0B") {
				integer, ok := new(big.Int).SetString(number, 0)
				if !ok {
					return nil, fmt.Errorf("invalid YAML float %q", node.Value)
				}
				return json.Number(integer.String()), nil
			}
			if strings.HasPrefix(number, ".") {
				number = "0" + number
			}
			if strings.HasPrefix(number, "-.") {
				number = "-0" + number[1:]
			}
			mantissaEnd := strings.IndexAny(number, "eE")
			if mantissaEnd < 0 {
				mantissaEnd = len(number)
			}
			mantissa := number[:mantissaEnd]
			fraction := ""
			if dot := strings.IndexByte(mantissa, '.'); dot >= 0 {
				fraction = mantissa[dot:]
				mantissa = mantissa[:dot]
			}
			{
				integer := mantissa
				negative := strings.HasPrefix(integer, "-")
				if negative {
					integer = integer[1:]
				}
				integer = strings.TrimLeft(integer, "0")
				if integer == "" {
					integer = "0"
				}
				if negative {
					integer = "-" + integer
				}
				number = integer + fraction + number[mantissaEnd:]
			}
			if index := strings.IndexAny(number, "eE"); index > 0 && number[index-1] == '.' {
				number = number[:index] + "0" + number[index:]
			}
			if strings.HasSuffix(number, ".") {
				number += "0"
			}
			if !json.Valid([]byte(number)) {
				return nil, fmt.Errorf("non-JSON YAML number %q", node.Value)
			}
			return json.Number(number), nil
		}
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported YAML node kind %d", node.Kind)
	}
}
