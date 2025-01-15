package main

import (
	"strings"
	"testing"
)

func TestIntegerBdecoding(t *testing.T) {
	tests := []string{"i3e", "i-3e", "i0e"}
	expected := []int{3, -3, 0}
	for i := 0; i < len(tests); i++ {
		data, err := decodeBecoding(strings.NewReader(tests[i]))
		if err != nil {
			t.Error(err)
			continue
		}
		got, ok := data.(int)
		if !ok {
			t.Errorf("expected %s to be of type int got %T", tests[i], data)
			continue
		}
		if got != expected[i] {
			t.Errorf("expected %s to be %v got %v", tests[i], expected[i], got)
		}
	}
}

func TestStringBdecoding(t *testing.T) {
	tests := []string{"5:hello"}
	expected := []string{"hello"}
	for i := 0; i < len(tests); i++ {
		data, err := decodeBecoding(strings.NewReader(tests[i]))
		if err != nil {
			t.Error(err)
			continue
		}
		got, ok := data.(string)
		if !ok {
			t.Errorf("expected %s to be of type string got %T", tests[i], data)
			continue
		}
		if got != expected[i] {
			t.Errorf("expected %s to be %v got %v", tests[i], expected[i], got)
		}
	}
}

func TestListBdecoding(t *testing.T) {
	tests := []string{"l5:helloe"}
	expected := [][]any{{"hello"}}
	for i := 0; i < len(tests); i++ {
		data, err := decodeBecoding(strings.NewReader(tests[i]))
		if err != nil {
			t.Error(err)
			continue
		}
		got, ok := data.([]any)
		if !ok {
			t.Errorf("expected %s to be of type []any got %T", tests[i], data)
			continue
		}
		if len(got) != len(expected[i]) {
			t.Errorf("expected %s to be of a slice of length %d got %d", tests[i], len(expected[i]), len(got))
			continue
		}
		for j := 0; j < len(got); j++ {
			if got[j] != expected[i][j] {
				t.Errorf("expected the %dth value of slice %v to be %v got %v", j, tests[i], expected[i][j], got[j])
				continue
			}
		}
	}
}

func TestDictBdecoding(t *testing.T) {
	tests := []string{"d5:hello5:worlde"}
	expected := []map[string]any{{"hello": "world"}}
	for i := 0; i < len(tests); i++ {
		data, err := decodeBecoding(strings.NewReader(tests[i]))
		if err != nil {
			t.Error(err)
			continue
		}
		got, ok := data.(map[string]any)
		if !ok {
			t.Errorf("expected %s to be of type map[string]any got %T", tests[i], data)
			continue
		}
		for k, v := range expected[i] {
			value, ok := got[k]
			if !ok {
				t.Errorf("expected %s to be of have key %s", tests[i], k)
				continue
			}
			if value != v {
				t.Errorf("expected %s to have the same value of key %s:%s instead got %s", tests[i], k, v, value)
			}
		}
	}
}
