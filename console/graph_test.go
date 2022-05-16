package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	netHTTP "net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang/mock/gomock"

	"akvorado/common/clickhousedb"
	"akvorado/common/daemon"
	"akvorado/common/helpers"
	"akvorado/common/http"
	"akvorado/common/reporter"
)

func TestGraphColumnSQLSelect(t *testing.T) {
	cases := []struct {
		Input    graphColumn
		Expected string
	}{
		{
			Input:    graphColumnSrcAddr,
			Expected: `IPv6NumToString(SrcAddr)`,
		}, {
			Input:    graphColumnDstAS,
			Expected: `concat(toString(DstAS), ': ', dictGetOrDefault('asns', 'name', DstAS, '???'))`,
		}, {
			Input:    graphColumnProto,
			Expected: `dictGetOrDefault('protocols', 'name', Proto, '???')`,
		}, {
			Input:    graphColumnEType,
			Expected: `if(EType = 0x800, 'IPv4', if(EType = 0x86dd, 'IPv6', '???'))`,
		}, {
			Input:    graphColumnOutIfSpeed,
			Expected: `toString(OutIfSpeed)`,
		}, {
			Input:    graphColumnExporterName,
			Expected: `ExporterName`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.Input.String(), func(t *testing.T) {
			got := tc.Input.toSQLSelect()
			if diff := helpers.Diff(got, tc.Expected); diff != "" {
				t.Errorf("toSQLWhere (-got, +want):\n%s", diff)
			}
		})
	}
}

func TestGraphQuerySQL(t *testing.T) {
	cases := []struct {
		Description string
		Input       graphQuery
		Expected    string
	}{
		{
			Description: "no dimensions, no filters",
			Input: graphQuery{
				Start:      time.Date(2022, 04, 10, 15, 45, 10, 0, time.UTC),
				End:        time.Date(2022, 04, 11, 15, 45, 10, 0, time.UTC),
				Points:     100,
				Dimensions: []graphColumn{},
				Filter:     graphFilter{},
			},
			Expected: `
WITH
 intDiv(864, {resolution})*{resolution} AS slot
SELECT
 toStartOfInterval(TimeReceived, INTERVAL slot second) AS time,
 SUM(Bytes*SamplingRate*8/slot) AS bps,
 emptyArrayString() AS dimensions
FROM {table}
WHERE {timefilter}
GROUP BY time, dimensions
ORDER BY time`,
		}, {
			Description: "no dimensions",
			Input: graphQuery{
				Start:      time.Date(2022, 04, 10, 15, 45, 10, 0, time.UTC),
				End:        time.Date(2022, 04, 11, 15, 45, 10, 0, time.UTC),
				Points:     100,
				Dimensions: []graphColumn{},
				Filter:     graphFilter{"DstCountry = 'FR' AND SrcCountry = 'US'"},
			},
			Expected: `
WITH
 intDiv(864, {resolution})*{resolution} AS slot
SELECT
 toStartOfInterval(TimeReceived, INTERVAL slot second) AS time,
 SUM(Bytes*SamplingRate*8/slot) AS bps,
 emptyArrayString() AS dimensions
FROM {table}
WHERE {timefilter} AND (DstCountry = 'FR' AND SrcCountry = 'US')
GROUP BY time, dimensions
ORDER BY time`,
		}, {
			Description: "no filters",
			Input: graphQuery{
				Start:  time.Date(2022, 04, 10, 15, 45, 10, 0, time.UTC),
				End:    time.Date(2022, 04, 11, 15, 45, 10, 0, time.UTC),
				Points: 100,
				Limit:  20,
				Dimensions: []graphColumn{
					graphColumnExporterName,
					graphColumnInIfProvider,
				},
				Filter: graphFilter{},
			},
			Expected: `
WITH
 intDiv(864, {resolution})*{resolution} AS slot,
 rows AS (SELECT ExporterName, InIfProvider FROM {table} WHERE {timefilter} GROUP BY ExporterName, InIfProvider ORDER BY SUM(Bytes) DESC LIMIT 20)
SELECT
 toStartOfInterval(TimeReceived, INTERVAL slot second) AS time,
 SUM(Bytes*SamplingRate*8/slot) AS bps,
 if((ExporterName, InIfProvider) IN rows, [ExporterName, InIfProvider], ['Other', 'Other']) AS dimensions
FROM {table}
WHERE {timefilter}
GROUP BY time, dimensions
ORDER BY time`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.Description, func(t *testing.T) {
			got, _ := tc.Input.toSQL()
			if diff := helpers.Diff(strings.Split(got, "\n"), strings.Split(tc.Expected, "\n")); diff != "" {
				t.Errorf("toSQL (-got, +want):\n%s", diff)
			}
		})
	}
}

func TestGraphHandler(t *testing.T) {
	r := reporter.NewMock(t)
	ch, mockConn := clickhousedb.NewMock(t, r)
	h := http.NewMock(t, r)
	c, err := New(r, Configuration{}, Dependencies{
		Daemon:       daemon.NewMock(t),
		HTTP:         h,
		ClickHouseDB: ch,
	})
	if err != nil {
		t.Fatalf("New() error:\n%+v", err)
	}
	helpers.StartStop(t, c)

	base := time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)
	expectedSQL := []struct {
		Time       time.Time `ch:"time"`
		Bps        float64   `ch:"bps"`
		Dimensions []string  `ch:"dimensions"`
	}{
		{base, 1000, []string{"router1", "provider1"}},
		{base, 2000, []string{"router1", "provider2"}},
		{base, 1200, []string{"router2", "provider2"}},
		{base, 1100, []string{"router2", "provider3"}},
		{base, 1900, []string{"Other", "Other"}},
		{base.Add(time.Minute), 500, []string{"router1", "provider1"}},
		{base.Add(time.Minute), 5000, []string{"router1", "provider2"}},
		{base.Add(time.Minute), 900, []string{"router2", "provider4"}},
		{base.Add(time.Minute), 100, []string{"Other", "Other"}},
		{base.Add(2 * time.Minute), 100, []string{"router1", "provider1"}},
		{base.Add(2 * time.Minute), 3000, []string{"router1", "provider2"}},
		{base.Add(2 * time.Minute), 100, []string{"router2", "provider4"}},
		{base.Add(2 * time.Minute), 100, []string{"Other", "Other"}},
	}
	expected := gin.H{
		// Sorted by sum of bps
		"rows": [][]string{
			{"router1", "provider2"}, // 10000
			{"router1", "provider1"}, // 1600
			{"router2", "provider2"}, // 1200
			{"router2", "provider3"}, // 1100
			{"router2", "provider4"}, // 1000
			{"Other", "Other"},       // 2100
		},
		"t": []string{
			"2009-11-10T23:00:00Z",
			"2009-11-10T23:01:00Z",
			"2009-11-10T23:02:00Z",
		},
		"points": [][]int{
			{2000, 5000, 3000},
			{1000, 500, 100},
			{1200, 0, 0},
			{1100, 0, 0},
			{0, 900, 100},
			{1900, 100, 100},
		},
		"min": []int{
			2000,
			100,
			0,
			0,
			0,
			100,
		},
		"max": []int{
			5000,
			1000,
			1200,
			1100,
			900,
			1900,
		},
		"average": []int{
			3333,
			533,
			400,
			366,
			333,
			700,
		},
	}
	mockConn.EXPECT().
		Select(gomock.Any(), gomock.Any(), gomock.Any()).
		SetArg(1, expectedSQL).
		Return(nil)

	input := graphQuery{
		Start:  time.Date(2022, 04, 10, 15, 45, 10, 0, time.UTC),
		End:    time.Date(2022, 04, 11, 15, 45, 10, 0, time.UTC),
		Points: 100,
		Limit:  20,
		Dimensions: []graphColumn{
			graphColumnExporterName,
			graphColumnInIfProvider,
		},
		Filter: graphFilter{"DstCountry = 'FR' AND SrcCountry = 'US'"},
	}
	payload := new(bytes.Buffer)
	err = json.NewEncoder(payload).Encode(input)
	if err != nil {
		t.Fatalf("Encode() error:\n%+v", err)
	}
	resp, err := netHTTP.Post(fmt.Sprintf("http://%s/api/v0/console/graph", h.Address),
		"application/json", payload)
	if err != nil {
		t.Fatalf("POST /api/v0/console/graph:\n%+v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("POST /api/v0/console/graph: got status code %d, not 200", resp.StatusCode)
	}
	gotContentType := resp.Header.Get("Content-Type")
	if gotContentType != "application/json; charset=utf-8" {
		t.Errorf("POST /api/v0/console/graph Content-Type (-got, +want):\n-%s\n+%s",
			gotContentType, "application/json; charset=utf-8")
	}
	decoder := json.NewDecoder(resp.Body)
	var got gin.H
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("POST /api/v0/console/graph error:\n%+v", err)
	}

	if diff := helpers.Diff(got, expected); diff != "" {
		t.Fatalf("POST /api/v0/console/graph (-got, +want):\n%s", diff)
	}
}

func TestGraphFieldsHandler(t *testing.T) {
	r := reporter.NewMock(t)
	ch, _ := clickhousedb.NewMock(t, r)
	h := http.NewMock(t, r)
	c, err := New(r, Configuration{}, Dependencies{
		Daemon:       daemon.NewMock(t),
		HTTP:         h,
		ClickHouseDB: ch,
	})
	if err != nil {
		t.Fatalf("New() error:\n%+v", err)
	}
	helpers.StartStop(t, c)

	resp, err := netHTTP.Get(fmt.Sprintf("http://%s/api/v0/console/graph/fields", h.Address))
	if err != nil {
		t.Fatalf("POST /api/v0/console/graph/fields:\n%+v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("POST /api/v0/console/graph/fields: got status code %d, not 200", resp.StatusCode)
	}
	gotContentType := resp.Header.Get("Content-Type")
	if gotContentType != "application/json; charset=utf-8" {
		t.Errorf("POST /api/v0/console/graph/fields Content-Type (-got, +want):\n-%s\n+%s",
			gotContentType, "application/json; charset=utf-8")
	}
	decoder := json.NewDecoder(resp.Body)
	var got []string
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("POST /api/v0/console/graph error:\n%+v", err)
	}
	expected := []string{
		"ExporterAddress",
		"ExporterName",
		"ExporterGroup",
		"SrcAddr",
		"DstAddr",
		"SrcAS",
		"DstAS",
		"SrcCountry",
		"DstCountry",
		"InIfName",
		"OutIfName",
		"InIfDescription",
		"OutIfDescription",
		"InIfSpeed",
		"OutIfSpeed",
		"InIfConnectivity",
		"OutIfConnectivity",
		"InIfProvider",
		"OutIfProvider",
		"InIfBoundary",
		"OutIfBoundary",
		"EType",
		"Proto",
		"SrcPort",
		"DstPort",
		"ForwardingStatus",
	}
	sort.Strings(expected)
	sort.Strings(got)

	if diff := helpers.Diff(got, expected); diff != "" {
		t.Fatalf("POST /api/v0/console/graph/fields (-got, +want):\n%s", diff)
	}
}