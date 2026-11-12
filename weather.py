#!/usr/bin/env python3
# weather — недельный прогноз без ключей и квот: api.met.no (yr.no).
# Проверено с резиденциального IP Москвы 2026-11-12: 0.4с, JSON ~38KB,
# open-meteo с того же IP был TCP-заблокирован, gismeteo — PROTOCOL_ERROR.
#
#   weather                 # Москва на 7 дней
#   weather химки           # город (справочник или lookup met.no)
#   weather 55.75,37.62     # координаты lat,lon
#   weather --days 3 spb
#   weather --json moscow   # для конвейера (poncho-ai notify и т.п.)
#
# Нюанс данных met.no (важно, нашли на живых): precipitation_amount в
# next_6_hours на ЧАСОВЫХ точках — это «прогноз на 6ч вперёд» (перекрывающиеся
# окна; сумма по ним дала 60 мм/день). Честный дневной итог: next_1_hours на
# часовых шагах + next_6_hours только там, где шаг серии сам 6-часовой.
import argparse, json, sys, urllib.request, urllib.parse
from datetime import datetime, timedelta, timezone

try:
    from zoneinfo import ZoneInfo
    TZ = ZoneInfo("Europe/Moscow")
except Exception:
    TZ = timezone(timedelta(hours=3))

UA = "mini-mcp-weather/1.0 (+https://github.com/ilkoid/mini-mcp; personal pipeline use)"
CITIES = {  # офлайн-справочник частых городов: (lat, lon)
    "moscow": (55.7558, 37.6173), "москва": (55.7558, 37.6173),
    "khimki": (55.8892, 37.4406), "химки": (55.8892, 37.4406),
    "spb": (59.9386, 30.3141), "питер": (59.9386, 30.3141),
    "санкт-петербург": (59.9386, 30.3141),
    "sochi": (43.5855, 39.7231), "сочи": (43.5855, 39.7231),
    "kazan": (55.7963, 49.1088), "казань": (55.7963, 49.1088),
    "ekb": (56.8389, 60.6057), "екатеринбург": (56.8389, 60.6057),
    "novosibirsk": (55.0084, 82.9357), "новосибирск": (55.0084, 82.9357),
}
SYMS = {
    "clearsky": "ясно", "fair": "ясно", "partlycloudy": "перем. облачн.",
    "cloudy": "облачно", "fog": "туман", "rain": "дождь", "lightrain": "неб. дождь",
    "heavyrain": "ливень", "rainshowers": "ливни", "lightrainshowers": "неб. ливни",
    "sleet": "снег с дождём", "lightsleet": "неб. снег с дождём",
    "snow": "снег", "lightsnow": "неб. снег", "heavysnow": "снегопад",
    "snowshowers": "снегопад", "lightsnowshowers": "неб. снегопад",
}


def http_json(url):
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.load(r)


def resolve(query):
    q = query.strip().lower()
    if "," in q:  # lat,lon
        lat, lon = (float(x) for x in q.split(",")[:2])
        return lat, lon, f"{lat:.4f},{lon:.4f}"
    if q in CITIES:
        lat, lon = CITIES[q]
        return lat, lon, q
    # геокодинг без ключей: координаты из статьи ru.wikipedia (met.no свой
    # location/2.0/name API retired; open-meteo-геокодер с RU-IP недоступен)
    url = ("https://ru.wikipedia.org/w/api.php?" + urllib.parse.urlencode(
        {"action": "query", "format": "json", "prop": "coordinates", "titles": query}))
    try:
        pages = http_json(url).get("query", {}).get("pages", {})
        for page in pages.values():
            coords = page.get("coordinates") or []
            if coords:
                lat, lon = coords[0]["lat"], coords[0]["lon"]
                return lat, lon, page.get("title", query)
    except Exception:
        pass
    sys.exit(f"не знаю город {query!r} (справочник: {' '.join(sorted(set(CITIES)))}), передай координаты lat,lon")


def forecast(lat, lon, days):
    url = (f"https://api.met.no/weatherapi/locationforecast/2.0/compact"
           f"?lat={lat}&lon={lon}")
    d = http_json(url)
    out, prev = {}, None
    for e in d["properties"]["timeseries"]:
        t = datetime.fromisoformat(e["time"]).astimezone(TZ)
        day = out.setdefault(t.date(), {"tmin": None, "tmax": None, "prec": 0.0, "sym": set()})
        temp = e["data"]["instant"]["details"].get("air_temperature")
        if temp is not None:
            day["tmin"] = temp if day["tmin"] is None else min(day["tmin"], temp)
            day["tmax"] = temp if day["tmax"] is None else max(day["tmax"], temp)
        step6 = prev is not None and (t - prev) >= timedelta(hours=5, minutes=30)
        n = e["data"].get("next_6_hours" if step6 else "next_1_hours")
        if n:  # окна без пересечений (см. комментарий в шапке)
            day["prec"] += n["details"].get("precipitation_amount") or 0
            sc = (n.get("summary") or {}).get("symbol_code")
            if sc:
                day["sym"].add(sc.split("_")[0])
        prev = t
    return d["properties"]["meta"]["updated_at"], [(k, out[k]) for k in sorted(out)[:days]]


def main():
    p = argparse.ArgumentParser(description="прогноз на N дней, api.met.no, без ключей")
    p.add_argument("place", nargs="?", default="москва", help="город или lat,lon")
    p.add_argument("-d", "--days", type=int, default=7)
    p.add_argument("--json", action="store_true", help="машинный вывод для конвейера")
    a = p.parse_args()
    lat, lon, name = resolve(a.place)
    updated, days = forecast(lat, lon, a.days)
    if a.json:
        payload = {
            "place": name, "lat": lat, "lon": lon, "model_updated": updated,
            "days": [{"date": d.isoformat(), "tmin": round(v["tmin"], 1),
                      "tmax": round(v["tmax"], 1), "precip_mm": round(v["prec"], 1),
                      "symbols": sorted(v["sym"])} for d, v in days],
        }
        print(json.dumps(payload, ensure_ascii=False))
        return
    print(f"{name} · api.met.no (модель {updated[:16]}Z)")
    for d, v in days:
        syms = ", ".join(sorted({SYMS.get(s, s) for s in v["sym"]}))
        print(f"{d.strftime('%d.%m %a')}  {v['tmin']:+.0f}…{v['tmax']:+.0f}°C   "
              f"осадки {v['prec']:4.1f} мм   {syms}")


if __name__ == "__main__":
    main()
