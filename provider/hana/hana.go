package hana

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SAP/go-hdb/driver"
	"github.com/go-spatial/geom"
	"github.com/go-spatial/geom/encoding/wkb"
	"github.com/go-spatial/tegola"
	"github.com/go-spatial/tegola/dict"
	"github.com/go-spatial/tegola/internal/log"
	"github.com/go-spatial/tegola/observability"
	"github.com/go-spatial/tegola/provider"
	"github.com/prometheus/client_golang/prometheus"
)

const Name = "hana"

type connectionPoolCollector struct {
	pool                     *sql.DB
	maxConnectionDesc        *prometheus.Desc
	currentConnectionsDesc   *prometheus.Desc
	availableConnectionsDesc *prometheus.Desc
}

func (c connectionPoolCollector) Close() {
	c.pool.Close()
}

func (c connectionPoolCollector) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return c.pool.PrepareContext(ctx, query)
}

func (c connectionPoolCollector) Query(query string) (*sql.Rows, error) {
	return c.pool.Query(query)
}

func (c connectionPoolCollector) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.pool.QueryContext(ctx, query, args...)
}

func (c connectionPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

func (c connectionPoolCollector) Collect(ch chan<- prometheus.Metric) {
	if c.pool == nil {
		return
	}
	stat := c.pool.Stats()
	ch <- prometheus.MustNewConstMetric(
		c.maxConnectionDesc,
		prometheus.GaugeValue,
		float64(stat.MaxOpenConnections),
	)
	ch <- prometheus.MustNewConstMetric(
		c.currentConnectionsDesc,
		prometheus.GaugeValue,
		float64(stat.OpenConnections),
	)
	ch <- prometheus.MustNewConstMetric(
		c.availableConnectionsDesc,
		prometheus.GaugeValue,
		float64(stat.MaxOpenConnections-stat.OpenConnections),
	)
}

func (c *connectionPoolCollector) Collectors(prefix string, _ func(configKey string) map[string]interface{}) ([]observability.Collector, error) {
	if c == nil {
		return nil, nil
	}
	if prefix != "" && !strings.HasSuffix(prefix, "_") {
		prefix = prefix + "_"
	}

	c.maxConnectionDesc = prometheus.NewDesc(
		prefix+"hana_max_connections",
		"Max number of hana connections in the pool",
		nil,
		nil,
	)

	c.currentConnectionsDesc = prometheus.NewDesc(
		prefix+"hana_current_connections",
		"Current number of hana connections in the pool",
		nil,
		nil,
	)

	c.availableConnectionsDesc = prometheus.NewDesc(
		prefix+"hana_available_connections",
		"Current number of available hana connections in the pool",
		nil,
		nil,
	)
	return []observability.Collector{c}, nil
}

// Provider provides the HANA data provider.
type Provider struct {
	pool *connectionPoolCollector
	// map of layer name and corresponding sql
	layers     map[string]Layer
	srid       uint64
	firstLayer string

	// collectorsRegistered keeps track if we have already collectorsRegistered these collectors
	// as the Collectors function will be called for each map and layer, but
	// we are going to assign those during runtime, instead of at registration
	// time; so we will only return these collectors on the first call.
	collectorsRegistered bool

	// Collectors for Query times
	mvtProviderQueryHistogramSeconds *prometheus.HistogramVec
	queryHistogramSeconds            *prometheus.HistogramVec
}

func (p *Provider) Collectors(prefix string, cfgFn func(configKey string) map[string]interface{}) ([]observability.Collector, error) {
	if p.collectorsRegistered {
		return nil, nil
	}

	buckets := []float64{.1, 1, 5, 20}
	collectors, err := p.pool.Collectors(prefix, cfgFn)
	if err != nil {
		return nil, err
	}

	p.mvtProviderQueryHistogramSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    prefix + "_mvt_provider_sql_query_seconds",
			Help:    "A histogram of query time for sql for mvt providers",
			Buckets: buckets,
		},
		[]string{"map_name", "z"},
	)

	p.queryHistogramSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    prefix + "_provider_sql_query_seconds",
			Help:    "A histogram of query time for sql for providers",
			Buckets: buckets,
		},
		[]string{"map_name", "layer_name", "z"},
	)

	p.collectorsRegistered = true
	return append(collectors, p.mvtProviderQueryHistogramSeconds, p.queryHistogramSeconds), nil
}

const (
	// We quote the field and table names to prevent colliding with postgres keywords.
	stdSQL = `SELECT %[1]v FROM %[2]v WHERE %[3]v.ST_IntersectsRectPlanar(ST_GeomFromText(?, ?), ST_GeomFromText(?, ?)) = 1`
	mvtSQL = `SELECT %[1]v FROM %[2]v`

	// SQL to get the column names, without hitting the information_schema. Though it might be better to hit the information_schema.
	fldsSQL = `SELECT * FROM %[1]v LIMIT 0;`
)

const (
	DefaultURI             = ""
	DefaultSRID            = tegola.WebMercator
	DefaultMaxConn         = 100
	DefaultMaxConnIdleTime = "30m"
	DefaultMaxConnLifetime = "1h"
)

const (
	ConfigKeyURI             = "uri"
	ConfigKeyEncrypt         = "encrypt"
	ConfigKeyMaxConn         = "max_connections"
	ConfigKeyMaxConnIdleTime = "max_connection_idle_time"
	ConfigKeyMaxConnLifetime = "max_connection_life_time"
	ConfigKeySRID            = "srid"
	ConfigKeyLayers          = "layers"
	ConfigKeyLayerName       = "name"
	ConfigKeyTablename       = "tablename"
	ConfigKeySQL             = "sql"
	ConfigKeyFields          = "fields"
	ConfigKeyGeomField       = "geometry_fieldname"
	ConfigKeyGeomIDField     = "id_fieldname"
	ConfigKeyGeomType        = "geometry_type"
)

// validateURI validates for minimum requirements for a valid postgresql uri
func validateURI(u string) error {
	uri, err := url.Parse(u)
	if err != nil {
		return ErrInvalidURI{Err: err}
	}

	if uri.Scheme != "postgres" && uri.Scheme != "postgresql" {
		return ErrInvalidURI{
			Msg: fmt.Sprintf("invalid connection scheme (%v)", uri.Scheme),
		}
	}

	if uri.User == nil {
		return ErrInvalidURI{Msg: "auth credentials missing"}
	}

	host, port, err := net.SplitHostPort(uri.Host)
	if err != nil {
		return ErrInvalidURI{
			Err: fmt.Errorf("splitting host port error: %w", err),
		}
	}

	if host == "" {
		return ErrInvalidURI{
			Msg: fmt.Sprintf("address %v:%v: missing host in address", host, port),
		}
	}

	if uri.Path == "" {
		return ErrInvalidURI{Msg: "missing database"}
	}

	return nil
}

// CreateConnection creates a connection from config values
func CreateConnection(config dict.Dicter) (*sql.DB, error) {
	uri, err := config.String(ConfigKeyURI, nil)
	if err != nil {
		return nil, err
	}

	url, err := url.Parse(uri)

	host := url.Host
	user := url.User.Username()
	password, ok := url.User.Password()

	if !ok {
		return nil, ErrInvalidURI{Msg: "user password is missing"}
	}

	connector := driver.NewBasicAuthConnector(
		host,
		user,
		password)

	query := url.Query()

	if query.Has(ConfigKeyEncrypt) {
		// TODO
		// tlsConfig := tls.Config{
		//	InsecureSkipVerify: false,
		//	ServerName:         HOST,
		//}

		//connector.SetTLSConfig(&tlsConfig)
	}

	max_conn := DefaultMaxConn
	if query.Has(ConfigKeyMaxConn) {
		max_conn, err = strconv.Atoi(query.Get(ConfigKeyMaxConn))
		if err != nil {
			return nil, err
		}
	}

	max_conn_idle_time, _ := time.ParseDuration(DefaultMaxConnIdleTime)
	if query.Has(ConfigKeyMaxConnIdleTime) {
		value := query.Get(ConfigKeyMaxConnIdleTime)
		max_conn_idle_time, err = time.ParseDuration(value)
		if err != nil {
			return nil, ErrInvalidURI{Msg: "max_connection_idle_time value is incorrect"}
		}
	}

	max_conn_life_time, _ := time.ParseDuration(DefaultMaxConnLifetime)
	if query.Has(ConfigKeyMaxConnLifetime) {
		value := query.Get(ConfigKeyMaxConnLifetime)
		max_conn_idle_time, err = time.ParseDuration(value)
		if err != nil {
			return nil, ErrInvalidURI{Msg: "max_conn_life_time value is incorrect"}
		}
	}

	db := sql.OpenDB(connector)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("Failed while establishing connection: %v", err)
	}

	db.SetMaxOpenConns(max_conn)
	db.SetConnMaxIdleTime(max_conn_idle_time)
	db.SetConnMaxLifetime(max_conn_life_time)

	return db, nil
}

// CreateProvider instantiates and returns a new HANA provider or an error.
// The function will validate that the config object looks good before
// trying to create a driver. This Provider supports the following fields
// in the provided map[string]interface{} map:
//
// 	uri (string): [Required] HANA database host
// 	srid (int): [Optional] The default SRID for the provider. Defaults to WebMercator (3857) but also supports WGS84 (4326)
// 	max_connections : [Optional] The max connections to maintain in the connection pool. Default is 100. 0 means no max.
// 	layers (map[string]struct{})  � This is map of layers keyed by the layer name. supports the following properties
//
// 		name (string): [Required] the name of the layer. This is used to reference this layer from map layers.
// 		tablename (string): [*Required] the name of the database table to query against. Required if sql is not defined.
// 		geometry_fieldname (string): [Optional] the name of the filed which contains the geometry for the feature. defaults to geom
// 		id_fieldname (string): [Optional] the name of the feature id field. defaults to gid
// 		fields ([]string): [Optional] a list of fields to include alongside the feature. Can be used if sql is not defined.
// 		srid (int): [Optional] the SRID of the layer. Supports 3857 (WebMercator) or 4326 (WGS84).
// 		sql (string): [*Required] custom SQL to use use. Required if tablename is not defined. Supports the following tokens:
//
// 			!BBOX! - [Required] will be replaced with the bounding box of the tile before the query is sent to the database.
// 			!ZOOM! - [Optional] will be replaced with the "Z" (zoom) value of the requested tile.
//
func CreateProvider(config dict.Dicter, providerType string) (*Provider, error) {
	conn, err := CreateConnection(config)
	if err != nil {
		return nil, err
	}

	srid := DefaultSRID
	if srid, err = config.Int(ConfigKeySRID, &srid); err != nil {
		return nil, err
	}

	p := Provider{
		srid: uint64(srid),
		pool: &connectionPoolCollector{pool: conn},
	}

	layers, err := config.MapSlice(ConfigKeyLayers)
	if err != nil {
		return nil, err
	}

	lyrs := make(map[string]Layer)
	lyrsSeen := make(map[string]int)

	for i, layer := range layers {

		lName, err := layer.String(ConfigKeyLayerName, nil)
		if err != nil {
			return nil, fmt.Errorf("For layer (%v) we got the following error trying to get the layer's name field: %w", i, err)
		}

		if j, ok := lyrsSeen[lName]; ok {
			return nil, fmt.Errorf("%v layer name is duplicated in both layer %v and layer %v", lName, i, j)
		}

		lyrsSeen[lName] = i
		if i == 0 {
			p.firstLayer = lName
		}

		fields, err := layer.StringSlice(ConfigKeyFields)
		if err != nil {
			return nil, fmt.Errorf("for layer (%v) %v %v field had the following error: %w", i, lName, ConfigKeyFields, err)
		}

		geomfld := "geom"
		geomfld, err = layer.String(ConfigKeyGeomField, &geomfld)
		if err != nil {
			return nil, fmt.Errorf("for layer (%v) %v : %w", i, lName, err)
		}

		idfld := ""
		idfld, err = layer.String(ConfigKeyGeomIDField, &idfld)
		if err != nil {
			return nil, fmt.Errorf("for layer (%v) %v : %w", i, lName, err)
		}
		if idfld == geomfld {
			return nil, fmt.Errorf("for layer (%v) %v: %v (%v) and %v field (%v) is the same", i, lName, ConfigKeyGeomField, geomfld, ConfigKeyGeomIDField, idfld)
		}

		geomType := ""
		geomType, err = layer.String(ConfigKeyGeomType, &geomType)
		if err != nil {
			return nil, fmt.Errorf("for layer (%v) %v : %w", i, lName, err)
		}

		var tblName string
		tblName, err = layer.String(ConfigKeyTablename, &lName)
		if err != nil {
			return nil, fmt.Errorf("for %v layer (%v) %v has an error: %w", i, lName, ConfigKeyTablename, err)
		}

		var sql string
		sql, err = layer.String(ConfigKeySQL, &sql)
		if err != nil {
			return nil, fmt.Errorf("for %v layer (%v) %v has an error: %w", i, lName, ConfigKeySQL, err)
		}

		if tblName != lName && sql != "" {
			log.Debugf("both %v and %v field are specified for layer (%v) %v, using only %[2]v field.", ConfigKeyTablename, ConfigKeySQL, i, lName)
		}

		var lsrid = srid
		if lsrid, err = layer.Int(ConfigKeySRID, &lsrid); err != nil {
			return nil, err
		}

		l := Layer{
			name:      lName,
			idField:   idfld,
			geomField: geomfld,
			srid:      uint64(lsrid),
		}

		if sql != "" && !isSelectQuery(sql) {
			// if it is not a SELECT query, then we assume we have a sub-query
			// (`(select ...) as foo`) which we can handle like a tablename
			tblName = sql
			sql = ""
		}

		if sql != "" {
			// convert !BOX! (MapServer) and !bbox! (Mapnik) to !BBOX! for compatibility
			sql := strings.Replace(strings.Replace(sql, "!BOX!", "!BBOX!", -1), "!bbox!", "!BBOX!", -1)
			// make sure that the sql has a !BBOX! token
			//if !strings.Contains(sql, bboxToken) {
			//	return nil, fmt.Errorf("SQL for layer (%v) %v is missing required token: %v", i, lName, bboxToken)
			//}
			if !strings.Contains(sql, "*") {
				if !strings.Contains(sql, geomfld) {
					return nil, fmt.Errorf("SQL for layer (%v) %v does not contain the geometry field: %v", i, lName, geomfld)
				}
				if !strings.Contains(sql, idfld) {
					return nil, fmt.Errorf("SQL for layer (%v) %v does not contain the id field for the geometry: %v", i, lName, sql)
				}
			}

			l.sql = sql
		} else {
			// Tablename and Fields will be used to build the query.
			// We need to do some work. We need to check to see Fields contains the geom and gid fields
			// and if not add them to the list. If Fields list is empty/nil we will use '*' for the field list.
			l.sql, err = genSQL(&l, p.pool, tblName, fields, true, providerType)
			if err != nil {
				return nil, fmt.Errorf("could not generate sql, for layer(%v): %w", lName, err)
			}
		}

		if debugLayerSQL {
			log.Debugf("SQL for Layer(%v):\n%v\n", lName, l.sql)
		}

		// set the layer geom type
		if geomType != "" {
			if err = p.setLayerGeomType(&l, geomType); err != nil {
				return nil, fmt.Errorf("error fetching geometry type for layer (%v): %w", l.name, err)
			}
		} else {
			if err = p.inspectLayerGeomType(&l); err != nil {
				return nil, fmt.Errorf("error fetching geometry type for layer (%v): %w", l.name, err)
			}
		}

		lyrs[lName] = l
	}
	p.layers = lyrs

	// track the provider so we can clean it up later
	providers = append(providers, p)

	return &p, nil
}

// setLayerGeomType sets the geomType field on the layer to one of point,
// linestring, polygon, multipoint, multilinestring, multipolygon or
// geometrycollection
func (p Provider) setLayerGeomType(l *Layer, geomType string) error {
	switch strings.ToLower(geomType) {
	case "point":
		l.geomType = geom.Point{}
	case "linestring":
		l.geomType = geom.LineString{}
	case "polygon":
		l.geomType = geom.Polygon{}
	case "multipoint":
		l.geomType = geom.MultiPoint{}
	case "multilinestring":
		l.geomType = geom.MultiLineString{}
	case "multipolygon":
		l.geomType = geom.MultiPolygon{}
	case "geometrycollection":
		l.geomType = geom.Collection{}
	default:
		return fmt.Errorf("unsupported geometry_type (%v) for layer (%v)", geomType, l.name)
	}
	return nil
}

// inspectLayerGeomType sets the geomType field on the layer by running the SQL
// and reading the geom type in the result set
func (p Provider) inspectLayerGeomType(l *Layer) error {
	return nil
}

// Layer fetches an individual layer from the provider, if it's configured
// if no name is provider, the first layer is returned
func (p *Provider) Layer(name string) (Layer, bool) {
	if name == "" {
		return p.layers[p.firstLayer], true
	}

	layer, ok := p.layers[name]
	return layer, ok
}

// Layers returns meta data about the various layers which are configured with the provider
func (p Provider) Layers() ([]provider.LayerInfo, error) {
	var ls []provider.LayerInfo

	for i := range p.layers {
		ls = append(ls, p.layers[i])
	}

	return ls, nil
}

// TileFeatures adheres to the provider.Tiler interface
func (p Provider) TileFeatures(ctx context.Context, layer string, tile provider.Tile, fn func(f *provider.Feature) error) error {

	var mapName string
	{
		mapNameVal := ctx.Value(observability.ObserveVarMapName)
		if mapNameVal != nil {
			// if it's not convertible to a string, we will ignore it.
			mapName, _ = mapNameVal.(string)
		}
	}
	// fetch the provider layer
	plyr, ok := p.Layer(layer)
	if !ok {
		return ErrLayerNotFound{layer}
	}

	srid := plyr.SRID()
	withBuffer := true

	minPt, maxPt, err := getTileBBox(srid, tile, withBuffer)

	sql, err := replaceTokens(plyr.sql, &plyr, tile, withBuffer)
	if err := ctxErr(ctx, err); err != nil {
		return fmt.Errorf("error replacing layer tokens for layer (%v) SQL (%v): %w", layer, sql, err)
	}

	if debugExecuteSQL {
		log.Debugf("TEGOLA_SQL_DEBUG:EXECUTE_SQL for layer (%v): %v", layer, sql)
	}

	// context check
	if err := ctx.Err(); err != nil {
		return err
	}

	now := time.Now()

	strLL := fmt.Sprintf("POINT(%g %g)", minPt.X(), minPt.Y())
	lobLL := &driver.Lob{}
	lobLL.SetReader(strings.NewReader(strLL))

	strUR := fmt.Sprintf("POINT(%g %g)", maxPt.X(), maxPt.Y())
	lobUR := &driver.Lob{}
	lobUR.SetReader(strings.NewReader(strUR))

	rows, err := p.pool.QueryContext(ctx, sql, lobLL, srid, lobUR, srid)

	if err := ctxErr(ctx, err); err != nil {
		return fmt.Errorf("error running layer (%v) SQL (%v): %w", layer, sql, err)
	}

	if p.queryHistogramSeconds != nil {
		z, _, _ := tile.ZXY()
		lbls := prometheus.Labels{
			"z":          strconv.FormatUint(uint64(z), 10),
			"map_name":   mapName,
			"layer_name": layer,
		}
		p.queryHistogramSeconds.With(lbls).Observe(time.Since(now).Seconds())
	}
	// when using ctxErr, it's import to make sure the defer rows.Close()
	// statement happens before the error check. The context may have been
	// canceled, but rows were also returned. If we don't close the rows
	// the the provider can't clean up the pool and the process will hang
	// trying to clean itself up.
	defer rows.Close()

	if err := ctxErr(ctx, err); err != nil {
		return fmt.Errorf("error running layer (%v) SQL (%v): %w", layer, sql, err)
	}

	columnDescs, err := rows.ColumnTypes()
	if err != nil {
		return err
	}

	// loop our field descriptions looking for the geometry field
	var geomFieldFound bool
	for i := range columnDescs {
		if columnDescs[i].Name() == plyr.GeomFieldName() {
			geomFieldFound = true
			break
		}
	}
	if !geomFieldFound {
		return ErrGeomFieldNotFound{
			GeomFieldName: plyr.GeomFieldName(),
			LayerName:     plyr.Name(),
		}
	}

	numColumns := len(columnDescs)
	rowBuffers := make([]*bytes.Buffer, numColumns)
	rowValues := make([]interface{}, numColumns)

	reportedLayerFieldName := ""
	for rows.Next() {
		// context check
		if err := ctx.Err(); err != nil {
			return err
		}

		setupRowValues(columnDescs, rowValues, rowBuffers)

		// fetch row values
		err := rows.Scan(rowValues...)
		if err := ctxErr(ctx, err); err != nil {
			return fmt.Errorf("error running layer (%v) SQL (%v): %w", layer, sql, err)
		}

		gid, geobytes, tags, err := readRowValues(ctx, plyr.GeomFieldName(), plyr.IDFieldName(), columnDescs, rowValues, rowBuffers)
		if err := ctxErr(ctx, err); err != nil {
			return fmt.Errorf("for layer (%v) %w", plyr.Name(), err)
		}

		// check that we have geometry data. if not, skip the feature
		if len(geobytes) == 0 {
			continue
		}

		// decode our WKB
		geometry, err := wkb.DecodeBytes(geobytes)
		if err != nil {
			switch err.(type) {
			case wkb.ErrUnknownGeometryType:
				rplfn := layer + ":" + plyr.GeomFieldName()
				// Only report to the log once. This is to prevent the logs from filling up if there are many geometries in the layer
				if reportedLayerFieldName == "" || reportedLayerFieldName == rplfn {
					reportedLayerFieldName = rplfn
					log.Warnf("Ignoring unsupported geometry in layer (%v). Only basic 2D geometry type are supported. Try using `ST_Force2D(%v)`.", layer, plyr.GeomFieldName())
				}
				continue
			default:
				return fmt.Errorf("unable to decode layer (%v) geometry field (%v) into wkb where (%v = %v): %w", layer, plyr.GeomFieldName(), plyr.IDFieldName(), gid, err)
			}
		}

		feature := provider.Feature{
			ID:       gid,
			Geometry: geometry,
			SRID:     plyr.SRID(),
			Tags:     tags,
		}

		// pass the feature to the provided callback
		if err = fn(&feature); err != nil {
			return err
		}
	}

	return rows.Err()
}

// Close will close the Provider's database connectio
func (p *Provider) Close() { p.pool.Close() }

// reference to all instantiated providers
var providers []Provider

// Cleanup will close all database connections and destroy all previously instantiated Provider instances
func Cleanup() {
	if len(providers) > 0 {
		log.Infof("cleaning up HANA providers")
	}

	for i := range providers {
		providers[i].Close()
	}

	providers = make([]Provider, 0)
}
