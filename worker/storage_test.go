package worker

import (
	"context"
	"testing"

	"github.com/calypr/funnel/events"
	"github.com/calypr/funnel/storage"
	"github.com/calypr/funnel/tes"
	"github.com/stretchr/testify/assert"
)

type fakelist struct {
	list []*storage.Object
	storage.Fake
}

func (f fakelist) List(context.Context, string) ([]*storage.Object, error) {
	return f.list, nil
}

func TestFlattenInputs(t *testing.T) {
	ev := events.NewTaskWriter("task-1", 0, events.Noop{})
	fake := fakelist{
		list: []*storage.Object{
			{URL: "s3://bkt/path/to/one.txt", Name: "path/to/one.txt"},
			{URL: "s3://bkt/path/to/two.txt", Name: "path/to/two.txt"},
		},
	}

	in := []*tes.Input{
		{Url: "s3://bkt/path", Path: "/inputs/foo", Type: tes.Directory},
		{Url: "s3://bkt/path", Path: "/inputs/foo/", Type: tes.Directory},
		{Url: "s3://bkt/path/", Path: "/inputs/foo", Type: tes.Directory},
		{Url: "s3://bkt/path/", Path: "/inputs/foo/", Type: tes.Directory},
	}

	bg := context.Background()
	flat, err := FlattenInputs(bg, in, fake, ev)
	if err != nil {
		t.Fatal(err)
	}

	expected := []*tes.Input{
		{Url: "s3://bkt/path/to/one.txt", Path: "/inputs/foo/to/one.txt"},
		{Url: "s3://bkt/path/to/two.txt", Path: "/inputs/foo/to/two.txt"},
		{Url: "s3://bkt/path/to/one.txt", Path: "/inputs/foo/to/one.txt"},
		{Url: "s3://bkt/path/to/two.txt", Path: "/inputs/foo/to/two.txt"},
		{Url: "s3://bkt/path/to/one.txt", Path: "/inputs/foo/to/one.txt"},
		{Url: "s3://bkt/path/to/two.txt", Path: "/inputs/foo/to/two.txt"},
		{Url: "s3://bkt/path/to/one.txt", Path: "/inputs/foo/to/one.txt"},
		{Url: "s3://bkt/path/to/two.txt", Path: "/inputs/foo/to/two.txt"},
	}

	assert.Equal(t, expected, flat)
}
