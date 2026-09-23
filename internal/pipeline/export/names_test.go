package export

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

const testJobID = "job_01J0000000000000007K2M9QXA"

func TestTimestampLabel(t *testing.T) {
	for _, tc := range []struct {
		sec  float64
		want string
	}{
		{0, "00:00"},
		{0.99, "00:00"},
		{6.2, "00:06"},
		{6.9, "00:06"},
		{5.9999999999, "00:06"}, // float error around a whole second
		{59.999, "00:59"},
		{60, "01:00"},
		{125.5, "02:05"},
		{3599.9, "59:59"},
		{3600, "1:00:00"},
		{3725.5, "1:02:05"},
		{36000, "10:00:00"},
		{-1, "00:00"},
		{math.Copysign(0, -1), "00:00"},
		{math.NaN(), "00:00"},
		{math.Inf(1), "00:00"},
		{math.Inf(-1), "00:00"},
	} {
		assert.Equal(t, tc.want, TimestampLabel(tc.sec), "%v", tc.sec)
	}
}

func TestFileName(t *testing.T) {
	for _, tc := range []struct {
		name      string
		domainKey string
		jobID     string
		index     int
		shotID    string
		ts        float64
		want      string
	}{
		{"plan example", "cars", testJobID, 1, "front_three_quarter", 6.4, "cars_7K2M9QXA_1_front_three_quarter_00-06.jpg"},
		{"over an hour", "cars", testJobID, 8, "interior_dashboard", 3725.5, "cars_7K2M9QXA_8_interior_dashboard_1-02-05.jpg"},
		{"shot sanitised", "cars", testJobID, 2, "Front Three-Quarter!", 12, "cars_7K2M9QXA_2_front_three-quarter_00-12.jpg"},
		{"unicode shot", "cars", testJobID, 3, "rück ansicht", 1, "cars_7K2M9QXA_3_r_ck_ansicht_00-01.jpg"},
		{"empty shot", "cars", testJobID, 4, "", 0, "cars_7K2M9QXA_4_other_00-00.jpg"},
		{"shot of junk only", "cars", testJobID, 5, "!!!", 0, "cars_7K2M9QXA_5_other_00-00.jpg"},
		{"negative timestamp", "cars", testJobID, 6, "other", -3, "cars_7K2M9QXA_6_other_00-00.jpg"},
		{"no path separators", "../cars", "job_a/b\\c..d", 7, "../../x", 1, "cars_a_b_c_d_7_x_00-01.jpg"},
		{"short job id", "realestate", "job_ABC", 1, "kitchen", 61, "realestate_ABC_1_kitchen_01-01.jpg"},
	} {
		assert.Equal(t, tc.want, FileName(tc.domainKey, tc.jobID, tc.index, tc.shotID, tc.ts), tc.name)
	}
}

func TestZipName(t *testing.T) {
	assert.Equal(t, "cars_7K2M9QXA_images.zip", ZipName("cars", testJobID))
	assert.Equal(t, "realestate_ABC_images.zip", ZipName("realestate", "job_ABC"))
	assert.Equal(t, "cars_x_y_images.zip", ZipName("cars/", "job_x/y"))
}
