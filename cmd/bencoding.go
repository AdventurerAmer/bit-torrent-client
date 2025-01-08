package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

func decodeBecoding(r io.Reader) (any, error) {
	p, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	data := string(p)
	result, _, err := decode(data)
	return result, err
}

func decode(s string) (any, string, error) {
	head := rune(s[0])
	switch head {
	case 'i':
		tail := strings.IndexRune(s, 'e')
		if tail == -1 {
			return nil, "", fmt.Errorf("invalid integer bencoding \"i<number>e\": missing \"e\"")
		}
		value, err := strconv.Atoi(s[1:tail])
		if err != nil {
			return nil, "", err
		}
		return value, s[tail+1:], nil
	case 'l', 'd':
		var value []any
		s = s[1:]
		for len(s) > 0 {
			if s[0] == 'e' {
				s = s[1:]
			} else {
				item, remaining, err := decode(s)
				if err != nil {
					return nil, "", err
				}
				value = append(value, item)
				s = remaining
			}
		}
		if head == 'd' {
			if len(value)%2 != 0 {
				fmt.Println(value)
				return nil, "", fmt.Errorf("invalid dictionary bencoding: number of keys doesn't match values")
			}
			dict := make(map[string]any)
			for i := 0; i < len(value); i += 2 {
				key, ok := value[i].(string)
				if !ok {
					return nil, "", fmt.Errorf("invalid dictionary bencoding: keys must be strings")
				}
				dict[key] = value[i+1]
			}
			return dict, "", nil
		}
		return value, "", nil
	default:
		if unicode.IsNumber(head) {
			colonIndex := strings.IndexRune(s, ':')
			if colonIndex == -1 {
				return nil, "", fmt.Errorf("invalid string bencoding \"<length>:<value>\": missing \":\"")
			}
			len, err := strconv.Atoi(s[:colonIndex])
			if err != nil {
				return nil, "", err
			}
			value := s[colonIndex+1 : colonIndex+1+len]
			return value, s[colonIndex+1+len:], nil
		} else {
			return nil, "", fmt.Errorf("unsupported bencoding")
		}
	}
}
