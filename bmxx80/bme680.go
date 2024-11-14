// Copyright 2024 The Periph Authors. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package bmxx80

import (
	"periph.io/x/conn/v3/physic"
)

// While the BME68x is a TPHG (temperature, pressure, humidity, gas) sensor, this implementation only supports TPH
// This also is limited to I2C support as SPI requires additional complexity in the form of page management

// sense68x reads the device's registers for bme680/bme688.
//
// It must be called with d.mu lock held.
func (d *Dev) sense68x(e *physic.Env) error {
	// All registers must be read in a single pass
	// Pressure: 0x1F~0x21
	// Temperature: 0x22~0x24
	// Humidity: 0x25~0x26
	buf := [8]byte{}
	b := buf[:]
	if !d.isBME {
		b = buf[:6]
	}
	if err := d.readReg(0x1F, b); err != nil {
		return err
	}
	// These values are 20 bits as per doc.
	pRaw := int32(buf[0])<<12 | int32(buf[1])<<4 | int32(buf[2])>>4
	tRaw := int32(buf[3])<<12 | int32(buf[4])<<4 | int32(buf[5])>>4

	t, tFine := d.cal68x.compensateTempInt(tRaw)
	// Convert CentiCelsius to Kelvin.
	e.Temperature = physic.Temperature(t)*10*physic.MilliCelsius + physic.ZeroCelsius

	if d.opts.Pressure != Off {
		p := d.cal68x.compensatePressureFloat(pRaw, tFine)
		// It has 8 bits of fractional Pascal.
		e.Pressure = physic.Pressure(p*256) * 15625 * physic.MicroPascal / 4
	}

	if d.opts.Humidity != Off {
		// This value is 16 bits as per doc.
		hRaw := int32(buf[6])<<8 | int32(buf[7])
		h := physic.RelativeHumidity(d.cal68x.compensateHumidityInt(hRaw, tFine))
		// Convert base 1024 to base 1000.
		e.Humidity = h * 10000 / 1024 * physic.MicroRH
	}

	return nil
}

func (d *Dev) isIdle68x() (bool, error) {
	// status
	v := [1]byte{}
	if err := d.readReg(0x1D, v[:]); err != nil {
		return false, err
	}
	// Make sure bit 5 (TPH) and 6(G) is cleared.
	return v[0]&60 == 0, nil
}

// newCalibration parses calibration data from both buffers.
func newCalibration68x(tph, h []byte) (c calibration68x) {
	c.t1 = uint16(h[31-23]) | uint16(h[32-23])<<8
	c.t2 = int16(tph[0]) | int16(tph[1])<<8
	c.t3 = int8(tph[2])
	c.p1 = uint16(tph[4]) | uint16(tph[5])<<8
	c.p2 = int16(tph[6]) | int16(tph[7])<<8
	c.p3 = int8(tph[8])
	c.p4 = int16(tph[10]) | int16(tph[11])<<8
	c.p5 = int16(tph[12]) | int16(tph[13])<<8
	c.p6 = int8(tph[15])
	c.p7 = int8(tph[14])
	c.p8 = int16(tph[18]) | int16(tph[19])<<8
	c.p9 = int16(tph[20]) | int16(tph[21])<<8
	c.p10 = tph[22]
	c.h1 = uint16(h[24-23])&0xF | uint16(h[25-23])<<4
	c.h2 = uint16(h[24-23])>>4 | uint16(h[23-23])<<4
	c.h3 = int8(h[26-23])
	c.h4 = int8(h[27-23])
	c.h5 = int8(h[28-23])
	c.h6 = h[29-23]
	c.h7 = int8(h[30-23])

	return c
}

type calibration68x struct {
	t1                 uint16
	t2                 int16
	t3                 int8
	p1                 uint16
	p2, p4, p5, p8, p9 int16
	p3, p6, p7         int8
	p10                uint8
	h1, h2             uint16
	h3, h4, h5, h7     int8
	h6                 uint8
}

// compensateTempInt returns temperature in °C, resolution is 0.01 °C.
// Output value of 5123 equals 51.23 C.
//
// raw has 20 bits of resolution.
// This function has been ported from
// https://github.com/boschsensortec/BME68x_SensorAPI/blob/80ea120a8b8ac987d7d79eb68a9ed796736be845/bme68x.c#L835
func (c *calibration68x) compensateTempInt(raw int32) (int32, int32) {
	var var1, var2, var3 int64
	var1 = (int64(raw) >> 3) - (int64(c.t1) << 1)
	var2 = (var1 * int64(c.t2)) >> 11
	var3 = ((var1 >> 1) * (var1 >> 1)) >> 12
	var3 = (var3 * (int64(c.t3) << 4)) >> 14
	tFine := var2 + var3
	return int32((tFine*5 + 128) >> 8), int32(tFine)
}

// compensatePressureFloat returns pressure in Pa. Output value of "96386.2"
// equals 96386.2 Pa = 963.862 hPa.
//
// raw has 20 bits of resolution.
func (c *calibration68x) compensatePressureFloat(raw, tFine int32) float64 {
	var var1, var2, var3, calc_pres float64
	var1 = (float64(tFine) / 2.0) - 64000.0
	var2 = var1 * var1 * ((float64(c.p6)) / 131072.0)
	var2 = var2 + (var1 * float64(c.p5) * 2.0)
	var2 = (var2 / 4.0) + (float64(c.p4) * 65536.0)
	var1 = (((float64(c.p3) * var1 * var1) / 16384.0) + (float64(c.p2) * var1)) / 524288.0
	var1 = (1.0 + (var1 / 32768.0)) * float64(c.p1)
	calc_pres = 1048576.0 - float64(raw)

	/* Avoid exception caused by division by zero */
	if int32(var1) != 0 {
		calc_pres = ((calc_pres - (var2 / 4096.0)) * 6250.0) / var1
		var1 = (float64(c.p9) * calc_pres * calc_pres) / 2147483648.0
		var2 = calc_pres * (float64(c.p8) / 32768.0)
		var3 = (calc_pres / 256.0) * (calc_pres / 256.0) * (calc_pres / 256.0) * (float64(c.p10) / 131072.0)
		calc_pres = calc_pres + (var1+var2+var3+(float64(c.p7)*128.0))/16.0
	} else {
		calc_pres = 0
	}

	return calc_pres
}

// compensateHumidityInt returns humidity in %RH in Q22.10 format (22 integer
// and 10 fractional bits). Output value of 47445 represents 47445/1024 =
// 46.333%
//
// raw has 16 bits of resolution.
// This function has been ported from
// https://github.com/boschsensortec/BME68x_SensorAPI/blob/80ea120a8b8ac987d7d79eb68a9ed796736be845/bme68x.c#L901
func (c *calibration68x) compensateHumidityInt(raw, tFine int32) uint32 {
	var var1, var2, var3, var4, var5, var6, tempScaled, calcHum int32
	tempScaled = ((tFine * 5) + 128) >> 8
	var1 = (raw - (int32(c.h1) * 16)) - (((tempScaled * int32(c.h3)) / 100) >> 1)
	var2 = (int32(c.h2) * (((tempScaled * int32(c.h4)) / 100) +
		(((tempScaled * ((tempScaled * int32(c.h5)) / 100)) >> 6) / 100) + (1 << 14))) >> 10
	var3 = var1 * var2
	var4 = int32(c.h6) << 7
	var4 = (var4 + ((tempScaled * int32(c.h7)) / 100)) >> 4
	var5 = ((var3 >> 14) * (var3 >> 14)) >> 10
	var6 = (var4 * var5) >> 1
	calcHum = (((var3 + var6) >> 10) * 1000) >> 12
	/* Cap at 100%rH */
	if calcHum > 100000 {
		calcHum = 100000
	} else if calcHum < 0 {
		calcHum = 0
	}

	return uint32(calcHum)
}
