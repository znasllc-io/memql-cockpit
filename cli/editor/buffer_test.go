package editor

import "testing"

func TestNewBuffer(t *testing.T) {
	buf := NewBuffer("line1\nline2\nline3", "test.memql", false)
	if buf.LineCount() != 3 {
		t.Errorf("expected 3 lines, got %d", buf.LineCount())
	}
	if buf.Line(0) != "line1" {
		t.Errorf("expected 'line1', got %q", buf.Line(0))
	}
	if buf.Line(2) != "line3" {
		t.Errorf("expected 'line3', got %q", buf.Line(2))
	}
}

func TestBufferInsertChar(t *testing.T) {
	buf := NewBuffer("hello", "test.memql", false)
	buf.InsertChar(0, 5, '!')
	if buf.Line(0) != "hello!" {
		t.Errorf("expected 'hello!', got %q", buf.Line(0))
	}

	buf.InsertChar(0, 0, '>')
	if buf.Line(0) != ">hello!" {
		t.Errorf("expected '>hello!', got %q", buf.Line(0))
	}
}

func TestBufferDeleteChar(t *testing.T) {
	buf := NewBuffer("hello", "test.memql", false)
	line, col := buf.DeleteChar(0, 5)
	if buf.Line(0) != "hell" {
		t.Errorf("expected 'hell', got %q", buf.Line(0))
	}
	if line != 0 || col != 4 {
		t.Errorf("expected cursor at (0,4), got (%d,%d)", line, col)
	}
}

func TestBufferDeleteCharMergeLines(t *testing.T) {
	buf := NewBuffer("line1\nline2", "test.memql", false)
	line, col := buf.DeleteChar(1, 0) // Backspace at start of line 2
	if buf.LineCount() != 1 {
		t.Errorf("expected 1 line after merge, got %d", buf.LineCount())
	}
	if buf.Line(0) != "line1line2" {
		t.Errorf("expected 'line1line2', got %q", buf.Line(0))
	}
	if line != 0 || col != 5 {
		t.Errorf("expected cursor at (0,5), got (%d,%d)", line, col)
	}
}

func TestBufferInsertNewline(t *testing.T) {
	buf := NewBuffer("hello world", "test.memql", false)
	buf.InsertNewline(0, 5)
	if buf.LineCount() != 2 {
		t.Errorf("expected 2 lines, got %d", buf.LineCount())
	}
	if buf.Line(0) != "hello" {
		t.Errorf("expected 'hello', got %q", buf.Line(0))
	}
	if buf.Line(1) != " world" {
		t.Errorf("expected ' world', got %q", buf.Line(1))
	}
}

func TestBufferReadOnly(t *testing.T) {
	buf := NewBuffer("immutable", "test.memql", true)
	buf.InsertChar(0, 0, 'X')
	if buf.Line(0) != "immutable" {
		t.Errorf("readonly buffer should not change, got %q", buf.Line(0))
	}
}

func TestBufferSource(t *testing.T) {
	source := "line1\nline2\nline3"
	buf := NewBuffer(source, "test.memql", false)
	if buf.Source() != source {
		t.Errorf("Source() mismatch: got %q", buf.Source())
	}
}
