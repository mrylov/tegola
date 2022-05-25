package hana

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/SAP/go-hdb/driver"
	"github.com/go-spatial/geom"
	"github.com/go-spatial/tegola"
	"github.com/go-spatial/tegola/basic"
	"github.com/go-spatial/tegola/provider"
)

// isMVT will return true if the provider is MVT based
func isMVT(providerType string) bool {
	return providerType == MVTProviderType
}

func quoteIdentifier(name string) string {
	if strings.Index(name, `"`) == 0 {
		return name
	}
	return fmt.Sprintf(`"%v"`, name)
}

// isSelectQuery is a regexp to check if a query starts with `SELECT`,
// case-insensitive and ignoring any preceeding whitespace and SQL comments.
var isSelectQueryRe = regexp.MustCompile(`(?i)^((\s*)(--.*\n)?)*select`)

func isSelectQuery(sql string) bool {
	return isSelectQueryRe.MatchString(sql)
}

func quoteTableName(name string) string {
	if strings.Contains(name, " ") {
		return name
	}

	strs := strings.Split(name, ".")
	nstrs := len(strs)
	if nstrs == 1 {
		return quoteIdentifier(strs[0])
	}

	ret := ""
	for i, s := range strs {
		ret = ret + quoteIdentifier(s)
		if i != nstrs-1 {
			ret = ret + "."
		}
	}

	return ret
}

func getGeomField(name string, providerType string) string {
	if isMVT(providerType) {
		return fmt.Sprintf(`%v AS %[1]v`, quoteIdentifier(name))
	} else {
		return fmt.Sprintf(`%v.ST_AsBinary()  AS %[1]v`, quoteIdentifier(name))
	}
}

// genSQL will fill in the SQL field of a layer given a pool, and list of fields.
func genSQL(l *Layer, pool *connectionPoolCollector, tblname string, flds []string, buffer bool, providerType string) (sql string, err error) {
	quotedTblName := quoteTableName(tblname)

	// we need to hit the database to see what the fields are.
	if len(flds) == 0 {
		sql := fmt.Sprintf(fldsSQL, quotedTblName)

		//	if a subquery is set in the 'sql' config the subquery is set to the layer's
		//	'tablename' param. because of this case normal SQL token replacement needs to be
		//	applied to tablename SQL generation
		tile := provider.NewTile(0, 0, 0, 64, tegola.WebMercator)
		sql, err = replaceTokens(sql, l, tile, buffer)
		if err != nil {
			return "", err
		}

		rows, err := pool.Query(sql)
		if err != nil {
			return "", err
		}
		defer rows.Close()

		columns, err := rows.Columns()
		if err != nil {
			return "", err
		}
		if len(columns) == 0 {
			return "", fmt.Errorf("no fields were returned for table %v", tblname)
		}

		// to avoid field names possibly colliding with Postgres keywords,
		// we wrap the field names in quotes
		for i := range columns {
			flds = append(flds, columns[i])
		}
	}

	fgeom := -1
	fid := -1

	for i, f := range flds {
		if f == l.idField {
			fid = i
		} else if f == l.geomField {
			fgeom = i
		}
		flds[i] = quoteIdentifier(flds[i])
	}

	// to avoid field names possibly colliding with Postgres keywords,
	// we wrap the field names in quotes

	if fgeom == -1 {
		flds = append(flds, getGeomField(l.geomField, providerType))
	} else {
		flds[fgeom] = getGeomField(l.geomField, providerType)
	}

	// add required id field
	if fid == -1 && l.idField != "" {
		flds = append(flds, quoteIdentifier(l.idField))
	}

	selectClause := strings.Join(flds, ", ")

	sqlTmpl := stdSQL

	if isMVT(providerType) {
		sqlTmpl = mvtSQL
	}

	return fmt.Sprintf(sqlTmpl, selectClause, quotedTblName, quoteIdentifier(l.geomField)), nil
}

const (
	zoomToken             = "!ZOOM!"
	xToken                = "!X!"
	yToken                = "!Y!"
	zToken                = "!Z!"
	scaleDenominatorToken = "!SCALE_DENOMINATOR!"
	pixelWidthToken       = "!PIXEL_WIDTH!"
	pixelHeightToken      = "!PIXEL_HEIGHT!"
	idFieldToken          = "!ID_FIELD!"
	geomFieldToken        = "!GEOM_FIELD!"
	geomTypeToken         = "!GEOM_TYPE!"
)

func getTileBBox(srid uint64, tile provider.Tile, withBuffer bool) (geom.Point, geom.Point, error) {
	var (
		extent *geom.Extent
	)

	if withBuffer {
		extent, _ = tile.BufferedExtent()
	} else {
		extent, _ = tile.Extent()
	}

	// TODO: leverage helper functions for minx / miny to make this easier to follow
	// TODO: it's currently assumed the tile will always be in WebMercator. Need to support different projections
	minGeo, err := basic.FromWebMercator(srid, geom.Point{extent.MinX(), extent.MinY()})
	if err != nil {
		return geom.Point{}, geom.Point{}, fmt.Errorf("Error trying to convert tile point: %w ", err)
	}

	maxGeo, err := basic.FromWebMercator(srid, geom.Point{extent.MaxX(), extent.MaxY()})
	if err != nil {
		return geom.Point{}, geom.Point{}, fmt.Errorf("Error trying to convert tile point: %w ", err)
	}

	return minGeo.(geom.Point), maxGeo.(geom.Point), nil
}

// replaceTokens replaces tokens in the provided SQL string
//
// !ZOOM! - the tile Z value
// !X! - the tile X value
// !Y! - the tile Y value
// !Z! - the tile Z value
// !SCALE_DENOMINATOR! - scale denominator, assuming 90.7 DPI (i.e. 0.28mm pixel size)
// !PIXEL_WIDTH! - the pixel width in meters, assuming 256x256 tiles
// !PIXEL_HEIGHT! - the pixel height in meters, assuming 256x256 tiles
// !GEOM_FIELD! - the geom field name
// !GEOM_TYPE! - the geom field type if defined otherwise ""
func replaceTokens(sql string, lyr *Layer, tile provider.Tile, withBuffer bool) (string, error) {
	var (
		extent  *geom.Extent
		geoType string
	)

	if lyr == nil {
		return "", ErrNilLayer
	}

	extent, _ = tile.Extent()
	// TODO: Always convert to meter if we support different projections
	pixelWidth := (extent.MaxX() - extent.MinX()) / 256
	pixelHeight := (extent.MaxY() - extent.MinY()) / 256
	scaleDenominator := pixelWidth / 0.00028 /* px size in m */

	if lyr.GeomType() != nil {
		geoType = fmt.Sprintf("%v", lyr.GeomType())
	}

	// replace query string tokens
	z, x, y := tile.ZXY()
	tokenReplacer := strings.NewReplacer(
		zoomToken, strconv.FormatUint(uint64(z), 10),
		zToken, strconv.FormatUint(uint64(z), 10),
		xToken, strconv.FormatUint(uint64(x), 10),
		yToken, strconv.FormatUint(uint64(y), 10),
		idFieldToken, lyr.IDFieldName(),
		geomFieldToken, lyr.GeomFieldName(),
		geomTypeToken, geoType,
		scaleDenominatorToken, strconv.FormatFloat(scaleDenominator, 'f', -1, 64),
		pixelWidthToken, strconv.FormatFloat(pixelWidth, 'f', -1, 64),
		pixelHeightToken, strconv.FormatFloat(pixelHeight, 'f', -1, 64),
	)

	uppercaseTokenSQL := uppercaseTokens(sql)

	return tokenReplacer.Replace(uppercaseTokenSQL), nil
}

var tokenRe = regexp.MustCompile("![a-zA-Z0-9_-]+!")

//	uppercaseTokens converts all !tokens! to uppercase !TOKENS!. Tokens can
//	contain alphanumerics, dash and underline chars.
func uppercaseTokens(str string) string {
	return tokenRe.ReplaceAllStringFunc(str, strings.ToUpper)
}

func setupRowValues(columnDescriptions []*sql.ColumnType, rowValues []interface{}, rowBuffers []*bytes.Buffer) {
	for i := range rowValues {
		rowBuffers[i] = nil

		switch columnDescriptions[i].DatabaseTypeName() {
		case "BLOB", "CLOB":
			rowBuffers[i] = &bytes.Buffer{}
			lob := &driver.Lob{}
			lob.SetWriter(rowBuffers[i])
			rowValues[i] = lob
			break
		case "NVARCHAR", "VARCHAR":
			rowValues[i] = new(interface{})
			break
		default:
			rowValues[i] = new(interface{})
			break
		}
	}
}

func readRowValues(ctx context.Context, geomFieldname, idFieldname string, descriptions []*sql.ColumnType, rowValues []interface{}, rowBuffers []*bytes.Buffer) (gid uint64, geom []byte, tags map[string]interface{}, err error) {
	var idFieldParsed bool
	tags = make(map[string]interface{})

	for i := range rowValues {

		// do a quick check
		if err := ctx.Err(); err != nil {
			return 0, nil, nil, err
		}

		// skip nil values.
		if rowValues[i] == nil {
			continue
		}

		desc := descriptions[i]
		fieldName := desc.Name()

		switch desc.DatabaseTypeName() {
		case "BLOB":
			if fieldName == geomFieldname {
				geom = make([]byte, rowBuffers[i].Len())
				_, err := rowBuffers[i].Read(geom)
				if err != nil {
					return 0, nil, nil, fmt.Errorf("unable to convert geometry field (%v) into bytes", geomFieldname)
				}
			} else {
				blob := make([]byte, rowBuffers[i].Len())
				_, err := rowBuffers[i].Read(blob)
				if err != nil {
					return 0, nil, nil, fmt.Errorf("unable to read blob field (%v)", fieldName)
				}
				tags[fieldName] = blob
			}
		case "NVARCHAR":
		case "VARCHAR":
			strValue := *(rowValues[i].(*string))
			if !idFieldParsed && fieldName == idFieldname {
				gid, err = convertToUInt64(strValue)
				if err != nil {
					return 0, nil, nil, err
				}
				idFieldParsed = true
				break
			}
			tags[fieldName] = strValue
			break
		default:
			value := *(rowValues[i].(*interface{}))
			if !idFieldParsed && fieldName == idFieldname {
				gid, err = convertToUInt64(value)
				if err != nil {
					return 0, nil, nil, err
				}
				idFieldParsed = true
			}
			tags[fieldName] = value
			break
		}
	}

	return gid, geom, tags, nil
}

func convertToUInt64(v interface{}) (intv uint64, err error) {
	switch aval := v.(type) {
	case float64:
		return uint64(aval), nil
	case int64:
		return uint64(aval), nil
	case uint64:
		return aval, nil
	case uint:
		return uint64(aval), nil
	case int8:
		return uint64(aval), nil
	case uint8:
		return uint64(aval), nil
	case uint16:
		return uint64(aval), nil
	case int32:
		return uint64(aval), nil
	case uint32:
		return uint64(aval), nil
	case string:
		return strconv.ParseUint(aval, 10, 64)
	default:
		return intv, fmt.Errorf("unable to convert field into a uint64")
	}
}

// ctxErr will check if the supplied context has an error (i.e. context canceled)
// and if so, return that error, else return the supplied error. This is useful
// as not all of Go's stdlib has adopted error wrapping so context.Canceled
// errors are not always easy to capture.
func ctxErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return err
}
