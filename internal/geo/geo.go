package geo

import "math"

const earthRadiusKm = 6371.0088

// Haversine 返回两点间大圆距离（公里）。输入为 WGS-84 经纬度。
func Haversine(lat1, lon1, lat2, lon2 float64) float64 {
	r1, r2 := rad(lat1), rad(lat2)
	dlat := rad(lat2 - lat1)
	dlon := rad(lon2 - lon1)
	a := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(r1)*math.Cos(r2)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 2 * earthRadiusKm * math.Asin(math.Min(1, math.Sqrt(a)))
}

func rad(d float64) float64 { return d * math.Pi / 180 }

// Bearing 返回从点 1 指向点 2 的方位角（度，正北为 0，顺时针；范围 -180~180）。
// 用来把「坐标移动了多远」说成「往东南 520 米」这种能对上的话。
func Bearing(lat1, lon1, lat2, lon2 float64) float64 {
	dLon := rad(lon2 - lon1)
	y := math.Sin(dLon) * math.Cos(rad(lat2))
	x := math.Cos(rad(lat1))*math.Sin(rad(lat2)) -
		math.Sin(rad(lat1))*math.Cos(rad(lat2))*math.Cos(dLon)
	return math.Atan2(y, x) * 180 / math.Pi
}

// 中国境内判定所用的粗略外接矩形
func inChina(lat, lon float64) bool {
	return lon >= 73.66 && lon <= 135.05 && lat >= 3.86 && lat <= 53.55
}

const (
	axxisPi   = math.Pi * 3000.0 / 180.0
	ee        = 0.00669342162296594323
	semiMajor = 6378245.0
)

// WGS84ToGCJ02 将 GPS 原始坐标转为火星坐标（GCJ-02）。
// 用于把原始定位、世界迷雾等 WGS-84 数据叠加到国内底图上。
func WGS84ToGCJ02(lat, lon float64) (float64, float64) {
	if !inChina(lat, lon) {
		return lat, lon
	}
	dLat := transformLat(lon-105.0, lat-35.0)
	dLon := transformLon(lon-105.0, lat-35.0)
	radLat := lat / 180.0 * math.Pi
	magic := math.Sin(radLat)
	magic = 1 - ee*magic*magic
	sqrtMagic := math.Sqrt(magic)
	dLat = (dLat * 180.0) / ((semiMajor * (1 - ee)) / (magic * sqrtMagic) * math.Pi)
	dLon = (dLon * 180.0) / (semiMajor / sqrtMagic * math.Cos(radLat) * math.Pi)
	return lat + dLat, lon + dLon
}

func transformLat(x, y float64) float64 {
	ret := -100.0 + 2.0*x + 3.0*y + 0.2*y*y + 0.1*x*y + 0.2*math.Sqrt(math.Abs(x))
	ret += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
	ret += (20.0*math.Sin(y*math.Pi) + 40.0*math.Sin(y/3.0*math.Pi)) * 2.0 / 3.0
	ret += (160.0*math.Sin(y/12.0*math.Pi) + 320*math.Sin(y*math.Pi/30.0)) * 2.0 / 3.0
	return ret
}

func transformLon(x, y float64) float64 {
	ret := 300.0 + x + 2.0*y + 0.1*x*x + 0.1*x*y + 0.1*math.Sqrt(math.Abs(x))
	ret += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
	ret += (20.0*math.Sin(x*math.Pi) + 40.0*math.Sin(x/3.0*math.Pi)) * 2.0 / 3.0
	ret += (150.0*math.Sin(x/12.0*math.Pi) + 300.0*math.Sin(x/30.0*math.Pi)) * 2.0 / 3.0
	return ret
}

// GCJ02ToWGS84 是 WGS84ToGCJ02 的逆运算。
// 正变换没有解析反函数，用不动点迭代逼近；偏移量随经纬度缓变，5 次迭代已收敛到厘米级。
func GCJ02ToWGS84(lat, lon float64) (float64, float64) {
	if !inChina(lat, lon) {
		return lat, lon
	}
	wLat, wLon := lat, lon
	for i := 0; i < 5; i++ {
		gLat, gLon := WGS84ToGCJ02(wLat, wLon)
		wLat += lat - gLat
		wLon += lon - gLon
	}
	return wLat, wLon
}

// GCJ02ToWGS84InPlace 与 GCJ02ToWGS84 相同，供批量换算时复用调用点。
func GCJ02ToWGS84InPlace(points [][2]float64) {
	for i := range points {
		points[i][0], points[i][1] = GCJ02ToWGS84(points[i][0], points[i][1])
	}
}
