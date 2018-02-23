package geojson

import (
	"reflect"
	"testing"

	"github.com/terranodo/tegola/geom"
)

func TestClosePolygon(t *testing.T) {
	type tcase struct {
		geom     geom.Polygon
		expected geom.Polygon
	}

	fn := func(t *testing.T, tc tcase) {
		t.Parallel()

		output := closePolygon(tc.geom)

		if !reflect.DeepEqual(tc.expected, output) {
			t.Errorf("expected %v got %v", tc.expected, output)
			return
		}
	}

	tests := map[string]tcase{
		"needs closing": {
			geom: geom.Polygon{
				{
					geom.Point{3.2, 4.3}, geom.Point{5.4, 6.5}, geom.Point{7.6, 8.7}, geom.Point{9.8, 10.9},
				},
			},
			expected: geom.Polygon{
				{
					geom.Point{3.2, 4.3}, geom.Point{5.4, 6.5}, geom.Point{7.6, 8.7}, geom.Point{9.8, 10.9}, geom.Point{3.2, 4.3},
				},
			},
		},
		"already closed": {
			geom: geom.Polygon{
				{
					geom.Point{3.2, 4.3}, geom.Point{5.4, 6.5}, geom.Point{7.6, 8.7}, geom.Point{9.8, 10.9}, geom.Point{3.2, 4.3},
				},
			},
			expected: geom.Polygon{
				{
					geom.Point{3.2, 4.3}, geom.Point{5.4, 6.5}, geom.Point{7.6, 8.7}, geom.Point{9.8, 10.9}, geom.Point{3.2, 4.3},
				},
			},
		},
	}

	for name, tc := range tests {
		tc := tc
		t.Run(name, func(t *testing.T) { fn(t, tc) })
	}
}
