package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/CloudyKit/jet"
	"github.com/Tomasen/realip"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/acme/autocert"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

var view *jet.Set
var mgoSession mgo.Session
var devices *mgo.Collection
var minerCollection *mgo.Collection
var lastBotInteraction time.Time
var miners map[string]time.Time
var iminers map[string]time.Time
var ipPriority []string
var botIP string
var connections map[string]*websocket.Conn

// Device : struct for devices
type Device struct {
	FriendCode uint64
	ID0        string `bson:"_id"`
	HasMovable bool
	HasPart1   bool
	HasAdded   bool
	WantsBF    bool
	LFCS       [8]byte
	MSed       [0x140]byte
	MSData     [12]byte
	ExpiryTime time.Time `bson:",omitempty"`
	CheckTime  time.Time
	Miner      string
	Expired    bool
	Cancelled  bool
}

// Miner : struct for tracking miners
type Miner struct {
	IP     string `bson:"_id"`
	Score  int
	Banned bool
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func buildMessage(command string) []byte {
	message := make(map[string]interface{})
	message["status"] = command
	message["minerCount"] = len(miners)
	c, err := devices.Find(bson.M{"haspart1": true, "wantsbf": true, "expirytime": time.Time{}}).Count()
	if err != nil {
		panic(err)
	}
	message["userCount"] = c
	b, err := devices.Find(bson.M{"hasmovable": bson.M{"$ne": true}, "haspart1": true, "wantsbf": true, "expirytime": bson.M{"$gt": time.Now()}, "expired": bson.M{"$ne": true}}).Count()
	if err != nil {
		panic(err)
	}
	message["miningCount"] = b
	a, err := devices.Find(bson.M{"haspart1": true}).Count()
	if err != nil {
		panic(err)
	}
	message["p1Count"] = a
	z, err := devices.Find(bson.M{"hasmovable": true}).Count()
	if err != nil {
		panic(err)
	}
	message["msCount"] = z
	n, err := devices.Count()
	if err != nil {
		panic(err)
	}
	message["totalCount"] = n
	data, err := json.Marshal(message)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func renderTemplate(template string, vars jet.VarMap, request *http.Request, writer http.ResponseWriter, context interface{}) {
	writer.Header().Add("Link", "</static/js/script.js>; rel=preload; as=script, <https://fonts.gstatic.com>; rel=preconnect, <https://fonts.googleapis.com>; rel=preconnect, <https://bootswatch.com>; rel=preconnect, <https://cdn.jsdelivr.net>; rel=preconnect,")
	t, err := view.GetTemplate(template)
	if err != nil {
		panic(err)
	}
	vars.Set("isUp", (lastBotInteraction.After(time.Now().Add(time.Minute * -5))))
	vars.Set("minerCount", len(miners))
	c, err := devices.Find(bson.M{"haspart1": true, "wantsbf": true, "expirytime": time.Time{}}).Count()
	if err != nil {
		panic(err)
	}
	vars.Set("userCount", c)
	b, err := devices.Find(bson.M{"hasmovable": bson.M{"$ne": true}, "haspart1": true, "wantsbf": true, "expirytime": bson.M{"$gt": time.Now()}}).Count()
	if err != nil {
		panic(err)
	}
	vars.Set("miningCount", b)
	a, err := devices.Find(bson.M{"haspart1": true}).Count()
	if err != nil {
		panic(err)
	}
	vars.Set("p1Count", a)
	z, err := devices.Find(bson.M{"hasmovable": true}).Count()
	if err != nil {
		panic(err)
	}
	vars.Set("msCount", z)
	n, err := devices.Count()
	if err != nil {
		panic(err)
	}
	vars.Set("totalCount", n)
	var tminers []bson.M
	q := minerCollection.Find(bson.M{"score": bson.M{"$gt": 0}}).Sort("-score").Limit(5)
	err = q.All(&tminers)
	if err != nil {
		panic(err)
	}
	vars.Set("miners", tminers)
	//log.Println(miners, len(miners))
	if err = t.Execute(writer, vars, nil); err != nil {
		// error when executing template
		panic(err)
	}
	if err != nil {
		panic(err)
	}
}

func logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Do stuff here
		log.Println(r.URL, r.Method, realip.FromRequest(r))
		// Call the next handler, which can be another middleware in the chain, or the final handler.
		next.ServeHTTP(w, r)
	})
}

func blacklist(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := minerCollection.Find(bson.M{"_id": realip.FromRequest(r), "banned": true}).Count()
		if c < 1 {
			next.ServeHTTP(w, r)
		} else {
			w.WriteHeader(403)
			w.Header().Add("X-Seedhelper-Banned", "true")
			w.Write([]byte("You have been banned from Seedhelper. This is probably because your script is glitching out. If you think you should be unbanned then find figgyc on Discord."))
		}
	})
}

func closer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/socket" {
			r.Close = true
		}
		next.ServeHTTP(w, r)
	})
}

func filetypeFixer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Do stuff here
		//log.Println(r)
		var tFile = regexp.MustCompile("\\.py$")
		if tFile.MatchString(r.RequestURI) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", "inline")
		}
		// Call the next handler, which can be another middleware in the chain, or the final handler.
		next.ServeHTTP(w, r)
	})
}

func reverse(numbers []byte) {
	for i, j := 0, len(numbers)-1; i < j; i, j = i+1, j-1 {
		numbers[i], numbers[j] = numbers[j], numbers[i]
	}
}

func checkIfID1(id1s string) bool {
	/*
		1) Take your id1
			24A90106478089A4534C303800035344

		2) Split it into u16 chunks
			24A9 0106 4780 89A4 534C 3038 0003 5344

		3) Reverse them backwards
			5344 0003 3038 534C 89A4 4780 0106 24A9

		4) Endian flip each of those chunks
			4453 0300 3830 4C53 A489 8047 0601 A924

		5) Shuffle them around with the table on 3dbrew (backwards!)
			0601 A924 A489 8047 3830 4C53 4453 0300

		6) Join them together
			0601A924A489804738304C5344530300 <-- cid
	*/
	//id1s := "24A90106478089A4534C303800035344"
	id1, err := hex.DecodeString(id1s)
	if err != nil {
		return true
	}
	var chunks [8][2]byte
	for i := 0; i < 8; i++ {
		chunks[i][0] = id1[i*2]
		chunks[i][1] = id1[(i*2)+1]
	}
	var rchunks [8][2]byte
	for i := 0; i < 8; i++ {
		rchunks[7-i] = chunks[i]
	}
	var echunks [8][2]byte
	for i := 0; i < 8; i++ {
		echunks[i][0] = rchunks[i][1]
		echunks[i][1] = rchunks[i][0]
	}
	var schunks [8][2]byte
	/* 3dbrew:
	Input CID u16 index	Output CID u16 index
	6					0
	7					1
	4					2
	5					3
	2					4
	3					5
	0					6
	1					7
	*/
	schunks[0] = echunks[6]
	schunks[1] = echunks[7]
	schunks[2] = echunks[4]
	schunks[3] = echunks[5]
	schunks[4] = echunks[2]
	schunks[5] = echunks[3]
	schunks[6] = echunks[0]
	schunks[7] = echunks[1]
	var cid [16]byte
	for i := 0; i < 8; i++ {
		cid[i*2] = schunks[i][0]
		cid[(i*2)+1] = schunks[i][1]
	}
	//hash := crc7.ComputeHash(cid[:])

	// pnm+oid should be valid ascii (<0x7F) but don't seem to be on most cards
	/*
		pnmoid := cid[1:7]
		for i := 0; i < 7; i++ {
			if pnmoid[i] > 0x7F {
				return false
			}
		}*/

	// zoogie said that he thinks this is reliable, idk but whatever
	return cid[15] == byte(0x00) && (cid[1] == byte(0x00) || cid[1] == byte(0x01))
}

func main() {
	log.SetFlags(log.Lshortfile)
	lastBotInteraction = time.Now()
	miners = map[string]time.Time{}
	iminers = map[string]time.Time{}
	ipPriority = strings.Split(os.Getenv("SEEDHELPER_IP_PRIORITY"), ",")
	botIP = os.Getenv("SEEDHELPER_BOT_IP")
	log.Println(ipPriority)
	// initialize mongo
	mgoSession, err := mgo.Dial("localhost")
	if err != nil {
		panic(err)
	}
	defer mgoSession.Close()

	devices = mgoSession.DB("main").C("devices")
	minerCollection = mgoSession.DB("main").C("miners")

	// init templates
	view = jet.NewHTMLSet("./views")
	// view.SetDevelopmentMode(true)

	// routing
	router := mux.NewRouter()

	router.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	router.Use(logger)
	router.Use(filetypeFixer)
	router.Use(blacklist)

	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		renderTemplate("home", make(jet.VarMap), r, w, nil)
	})

	router.HandleFunc("/logo.png", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "logo.png")
	})

	// client:
	connections = make(map[string]*websocket.Conn)
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	router.HandleFunc("/socket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println(err)
			return
		}
		//... Use conn to send and receive messages.
		for {
			messageType, p, err := conn.ReadMessage()
			if err != nil {
				log.Println("disconnection?", err)
				return
			}
			if messageType == websocket.TextMessage {
				var object map[string]interface{}
				err := json.Unmarshal(p, &object)
				if err != nil {
					log.Println(err)
					//return
					for k, v := range connections {
						if v == conn {
							delete(connections, k)
						}
					}
					return
				}
				if object["id0"] == nil {
					//return
					continue
				}
				log.Println("identify:", realip.FromRequest(r), object["id0"])
				//log.Println(object["part1"], "packet")
				/*isRegistered := false
				for _, v := range connections {
					if v == conn {
						isRegistered = true
					}
				}
				if isRegistered == false {*/
				connections[object["id0"].(string)] = conn
				//}

				if object["request"] == "bruteforce" {
					// add to BF pool
					err := devices.Update(bson.M{"_id": object["id0"].(string)}, bson.M{"$set": bson.M{"wantsbf": true, "expirytime": time.Time{}}})
					if err != nil {
						log.Println(err)
						//return
					}
				} else if object["request"] == "cancel" {
					// canseru jobbu
					err := devices.Update(bson.M{"_id": object["id0"].(string)}, bson.M{"cancelled": true})
					if err != nil {
						w.Write([]byte("error"))
						return
					}
				} else if object["part1"] != nil {
					// add to work pool

					c, err := devices.Find(bson.M{"_id": object["id0"], "expired": true}).Count()
					if err != nil || c > 0 {
						if err := conn.WriteMessage(websocket.TextMessage, buildMessage("flag")); err != nil {
							log.Println(err)
							return
						}
						continue
					}
					valid := true
					if regexp.MustCompile("[0-9a-fA-F]{32}").MatchString(object["id0"].(string)) == false {
						valid = false
					}

					if valid == false {
						if err := conn.WriteMessage(websocket.TextMessage, buildMessage("friendCodeInvalid")); err != nil {
							log.Println(err)
							return
						}
						continue
					}
					p1Slice, err := base64.StdEncoding.DecodeString(object["part1"].(string))
					if err != nil {
						log.Println(err)
						return
					}
					lfcsSlice := p1Slice[:8]
					reverse(lfcsSlice)
					var lfcsArray [8]byte
					copy(lfcsArray[:], lfcsSlice[:])
					var checkArray [8]byte
					if lfcsArray == checkArray {
						if err := conn.WriteMessage(websocket.TextMessage, buildMessage("friendCodeInvalid")); err != nil {
							log.Println(err)
							return
						}
						continue
					}
					if object["defoID0"] != "yes" && checkIfID1(object["id0"].(string)) {
						if err := conn.WriteMessage(websocket.TextMessage, buildMessage("couldBeID1")); err != nil {
							log.Println(err)
							return
						}
						continue
					}
					device := bson.M{"lfcs": lfcsArray, "haspart1": true, "hasadded": true, "wantsbf": true, "expirytime": time.Time{}}
					_, err = devices.Upsert(bson.M{"_id": object["id0"].(string)}, device)
					if err != nil {
						log.Println(err)
						//return
					}
					if err := conn.WriteMessage(websocket.TextMessage, buildMessage("queue")); err != nil {
						log.Println(err)
						//return
					}
				} else if object["friendCode"] != nil {
					// add to bot pool

					c, err := devices.Find(bson.M{"_id": object["id0"], "expired": true}).Count()
					if err != nil || c > 0 {
						if err := conn.WriteMessage(websocket.TextMessage, buildMessage("flag")); err != nil {
							log.Println(err)
							return
						}
						continue
					}
					/*
						based on https://github.com/ihaveamac/Kurisu/blob/master/addons/friendcode.py#L24
						    def verify_fc(self, fc):
								fc = int(fc.replace('-', ''))
								if fc > 0x7FFFFFFFFF:
									return None
								principal_id = fc & 0xFFFFFFFF
								checksum = (fc & 0xFF00000000) >> 32
								return (fc if hashlib.sha1(struct.pack('<L', principal_id)).digest()[0] >> 1 == checksum else None)
					*/
					valid := true
					fc, err := strconv.Atoi(object["friendCode"].(string))
					if err != nil {
						valid = false
					}
					if fc > 0x7FFFFFFFFF {
						valid = false
					}
					if fc == 27599290078 { // the bot
						valid = false
					}
					principalID := fc & 0xFFFFFFFF
					checksum := (fc & 0xFF00000000) >> 32

					pidb := make([]byte, 4)
					binary.LittleEndian.PutUint32(pidb, uint32(principalID))
					if int(sha1.Sum(pidb)[0])>>1 != checksum {
						valid = false
					}

					if regexp.MustCompile("[0-9a-fA-F]{32}").MatchString(object["id0"].(string)) == false {
						valid = false
					}

					if valid == false {
						if err := conn.WriteMessage(websocket.TextMessage, buildMessage("friendCodeInvalid")); err != nil {
							log.Println(err)
							return
						}
						continue
					}
					if object["defoID0"] != "yes" && checkIfID1(object["id0"].(string)) == true {
						if err := conn.WriteMessage(websocket.TextMessage, buildMessage("couldBeID1")); err != nil {
							log.Println(err)
							return
						}
						continue
					}
					log.Println(fc)
					device := bson.M{"friendcode": uint64(fc), "hasadded": false, "haspart1": false}
					_, err = devices.Upsert(bson.M{"_id": object["id0"].(string)}, device)
					if err != nil {
						log.Println(err)
						//return
					}
					if err := conn.WriteMessage(websocket.TextMessage, buildMessage("friendCodeProcessing")); err != nil {
						log.Println(err)
						//return
					}

				} else {
					// checc
					//log.Println("check")
					query := devices.Find(bson.M{"_id": object["id0"].(string)})
					count, err := query.Count()
					if err != nil {
						log.Println(err)
						//return
					}
					if count > 0 {
						var device Device
						err = query.One(&device)
						if err != nil {
							log.Println(err)
							//return
						}
						if device.HasMovable == true {
							if err := conn.WriteMessage(websocket.TextMessage, buildMessage("done")); err != nil {
								log.Println(err)
								//return
							}
						} else if (device.ExpiryTime != time.Time{}) {
							if err := conn.WriteMessage(websocket.TextMessage, buildMessage("bruteforcing")); err != nil {
								log.Println(err)
								//return
							}
						} else if device.WantsBF == true {
							if err := conn.WriteMessage(websocket.TextMessage, buildMessage("queue")); err != nil {
								log.Println(err)
								//return
							}
						} else if device.HasPart1 == true {
							if err := conn.WriteMessage(websocket.TextMessage, buildMessage("movablePart1")); err != nil {
								log.Println(err)
								//return
							}
						} else if device.HasAdded == true {
							if err := conn.WriteMessage(websocket.TextMessage, buildMessage("friendCodeAdded")); err != nil {
								log.Println(err)
								//return
							}
						} else {
							if err := conn.WriteMessage(websocket.TextMessage, buildMessage("friendCodeProcessing")); err != nil {
								log.Println(err)
								//return
							}
						}
					} else {
						log.Println("empty id0 to socket, dropped DB?")
						//return
					}
				}
			} else if messageType == websocket.CloseMessage {
				for k, v := range connections {
					if v == conn {
						delete(connections, k)
					}
				}
			}

		}
	})

	// part1 auto script:
	// /getfcs
	router.HandleFunc("/getfcs", func(w http.ResponseWriter, r *http.Request) {
		if realip.FromRequest(r) != botIP {
			w.Write([]byte("nothing"))
			return
		}
		lastBotInteraction = time.Now()

		query := devices.Find(bson.M{"hasadded": false})
		count, err := query.Count()
		if err != nil || count < 1 {
			w.Write([]byte("nothing"))
			return
		}
		var aDevices []Device
		err = query.All(&aDevices)
		if err != nil || len(aDevices) < 1 {
			w.Write([]byte("nothing"))
			return
		}
		for _, device := range aDevices {
			w.Write([]byte(strconv.FormatUint(device.FriendCode, 10)))
			w.Write([]byte("\n"))
		}
		return
	})
	// /added/fc
	router.HandleFunc("/added/{fc}", func(w http.ResponseWriter, r *http.Request) {
		if realip.FromRequest(r) != botIP {
			w.Write([]byte("fail"))
			return
		}
		b := mux.Vars(r)["fc"]
		a, err := strconv.Atoi(b)
		if err != nil {
			w.Write([]byte("fail"))
			log.Println(err)
			return
		}
		fc := uint64(a)

		//log.Println(r, &r)

		err = devices.Update(bson.M{"friendcode": fc, "hasadded": false}, bson.M{"$set": bson.M{"hasadded": true}})
		if err != nil { // && err != mgo.ErrNotFound {
			w.Write([]byte("fail"))
			log.Println("a", err)
			return
		}

		query := devices.Find(bson.M{"friendcode": fc})
		var device Device
		err = query.One(&device)
		if err != nil {
			w.Write([]byte("fail"))
			log.Println("x", err)
			return
		}
		for id0, conn := range connections {
			//log.Println(id0, device.ID0, "hello!")
			if id0 == device.ID0 {
				if err := conn.WriteMessage(websocket.TextMessage, buildMessage("friendCodeAdded")); err != nil {
					delete(connections, id0)
					//w.Write([]byte("fail"))
					log.Println(err)
					//return
				}
			}
		}
		w.Write([]byte("success"))

	})

	// /lfcs/fc
	// get param lfcs is lfcs as hex eg 34cd12ab or whatevs
	router.HandleFunc("/lfcs/{fc}", func(w http.ResponseWriter, r *http.Request) {
		if realip.FromRequest(r) != botIP {
			w.Write([]byte("fail"))
			return
		}
		b := mux.Vars(r)["fc"]
		a, err := strconv.Atoi(b)
		if err != nil {
			w.Write([]byte("fail"))
			log.Println(err)
			return
		}
		fc := uint64(a)

		lfcs, ok := r.URL.Query()["lfcs"]
		if ok == false {
			log.Println("wot")
			w.Write([]byte("fail"))
			return
		}

		sliceLFCS, err := hex.DecodeString(lfcs[0])
		if err != nil {
			w.Write([]byte("fail"))
			log.Println(err)
			return
		}
		var x [8]byte
		copy(x[:], sliceLFCS)
		x[0] = 0x00
		x[1] = 0x00
		x[2] = 0x00
		log.Println(fc, a, b, lfcs, x, sliceLFCS)
		err = devices.Update(bson.M{"friendcode": fc, "haspart1": false}, bson.M{"$set": bson.M{"haspart1": true, "lfcs": x}})
		if err != nil && err != mgo.ErrNotFound {
			w.Write([]byte("fail"))
			log.Println(err)
			return
		}

		query := devices.Find(bson.M{"friendcode": fc})
		var device Device
		err = query.One(&device)
		if err != nil {
			log.Println(err)
			w.Write([]byte("fail"))
			log.Println("las")
			return
		}
		for id0, conn := range connections {
			if id0 == device.ID0 {
				if err := conn.WriteMessage(websocket.TextMessage, buildMessage("movablePart1")); err != nil {
					log.Println(err)
					delete(connections, id0)
					//w.Write([]byte("fail"))
					//return
				}
			}
		}

		w.Write([]byte("success"))
		log.Println("last")

	})

	// msed auto script:
	// /cancel/id0
	router.HandleFunc("/cancel/{id0}", func(w http.ResponseWriter, r *http.Request) {
		id0 := mux.Vars(r)["id0"]
		log.Println(id0)
		kill := false

		yn, ok := r.URL.Query()["kill"]
		if ok == true {
			if yn[0] == "n" {
				kill = true
			}
		}

		err := devices.Update(bson.M{"_id": id0}, bson.M{"$set": bson.M{"wantsbf": kill, "expired": (yn[0] == "y"), "expirytime": time.Time{}}})
		if err != nil {
			w.Write([]byte("error"))
			return
		}
		w.Write([]byte("success"))

		for id01, conn := range connections {
			if id0 == id01 {
				if err := conn.WriteMessage(websocket.TextMessage, buildMessage("flag")); err != nil {
					log.Println(err)
					delete(connections, id0)
					return
				}
			}
		}

	})

	// /setname
	router.HandleFunc("/setname", func(w http.ResponseWriter, r *http.Request) {
		names, ok := r.URL.Query()["name"]
		name := names[0]
		if ok == false || name == "" {
			w.Write([]byte("specify a name"))
			return
		}
		c, err := minerCollection.Find(bson.M{"_id": bson.M{"$ne": realip.FromRequest(r)}, "name": name}).Count()
		if err != nil || c != 0 {
			w.Write([]byte("name taken"))
			return
		}
		_, err = minerCollection.Upsert(bson.M{"_id": realip.FromRequest(r)}, bson.M{"$set": bson.M{"name": name}})
		if err != nil {
			w.Write([]byte("error"))
			w.Write([]byte(err.Error()))
			log.Println(err)
		} else {
			w.Write([]byte("success"))
		}
	})

	// /getwork
	router.HandleFunc("/getwork", func(w http.ResponseWriter, r *http.Request) {
		miners[realip.FromRequest(r)] = time.Now()
		iminers[realip.FromRequest(r)] = time.Now()
		ok, err := devices.Find(bson.M{"miner": realip.FromRequest(r), "hasmovable": bson.M{"$ne": true}, "expirytime": bson.M{"$ne": time.Time{}}, "expired": bson.M{"$ne": true}}).Count()
		if ok > 0 {
			w.Write([]byte("nothing"))
			return
		}
		query := devices.Find(bson.M{"haspart1": true, "wantsbf": true, "expirytime": bson.M{"$eq": time.Time{}}, "expired": bson.M{"$ne": true}})
		count, err := query.Count()
		if err != nil || count < 1 {
			w.Write([]byte("nothing"))
			return
		}
		var aDevice Device
		err = query.One(&aDevice)
		if err != nil {
			w.Write([]byte("nothing"))
			log.Println(err)
			return
		}
		w.Write([]byte(aDevice.ID0))
	})
	// /claim/id0
	router.HandleFunc("/claim/{id0}", func(w http.ResponseWriter, r *http.Request) {
		ok, err := devices.Find(bson.M{"miner": realip.FromRequest(r), "hasmovable": bson.M{"$ne": true}, "expirytime": bson.M{"$ne": time.Time{}}}).Count()
		if ok > 0 {
			w.Write([]byte("nothing"))
			return
		}
		id0 := mux.Vars(r)["id0"]
		//log.Println(id0)
		err = devices.Update(bson.M{"_id": id0}, bson.M{"$set": bson.M{"expirytime": time.Now().Add(time.Hour), "miner": realip.FromRequest(r)}})
		if err != nil {
			log.Println(err)
			return
		}
		w.Write([]byte("success"))
		miners[realip.FromRequest(r)] = time.Now()
		for id02, conn := range connections {
			//log.Println(id0, device.ID0, "hello!")
			if id02 == id0 {
				if err := conn.WriteMessage(websocket.TextMessage, buildMessage("bruteforcing")); err != nil {
					delete(connections, id0)
					//w.Write([]byte("fail"))
					log.Println(err)
					//return
				}
			}
		}
	})
	// /part1/id0
	// this is also used by client if they want self BF so /claim is needed
	router.HandleFunc("/part1/{id0}", func(w http.ResponseWriter, r *http.Request) {
		id0 := mux.Vars(r)["id0"]
		query := devices.Find(bson.M{"_id": id0})
		count, err := query.Count()
		if err != nil || count < 1 {
			w.Write([]byte("error"))
			log.Println("z", err, count)
			return
		}
		var device Device
		err = query.One(&device)
		if err != nil || device.HasPart1 == false {
			w.Write([]byte("error"))
			log.Println("a", err)
			return
		}
		buf := bytes.NewBuffer(make([]byte, 0, 0x1000))
		leLFCS := make([]byte, 8)
		binary.BigEndian.PutUint64(leLFCS, binary.LittleEndian.Uint64(device.LFCS[:]))
		_, err = buf.Write(leLFCS)
		if err != nil {
			w.Write([]byte("error"))
			log.Println("b", err)
			return
		}
		_, err = buf.Write(make([]byte, 0x8))
		if err != nil {
			w.Write([]byte("error"))
			log.Println("c", err)
			return
		}
		_, err = buf.Write([]byte(device.ID0))
		if err != nil {
			w.Write([]byte("error"))
			log.Println("d", err)
			return
		}
		_, err = buf.Write(make([]byte, 0xFD0))
		if err != nil {
			w.Write([]byte("error"))
			log.Println("e", err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "inline; filename=\"movable_part1.sed\"")
		w.Write(buf.Bytes())
	})
	// /check/id0
	// allows user cancel and not overshooting the 1hr job max time
	router.HandleFunc("/check/{id0}", func(w http.ResponseWriter, r *http.Request) {
		id0 := mux.Vars(r)["id0"]
		query := devices.Find(bson.M{"_id": id0, "haspart1": true, "hasmovable": bson.M{"$ne": true}, "wantsbf": true, "miner": realip.FromRequest(r), "expirytime": bson.M{"$gt": time.Now()}})
		count, err := query.Count()
		if err != nil || count < 1 {
			w.Write([]byte("error"))
			log.Println("z", err, count)
			return
		}
		devices.Update(bson.M{"_id": id0}, bson.M{"$set": bson.M{"checktime": time.Now().Add(time.Minute)}})
		miners[realip.FromRequest(r)] = time.Now()
		w.Write([]byte("ok"))
	})
	// /movable/id0
	router.HandleFunc("/movable/{id0}", func(w http.ResponseWriter, r *http.Request) {
		id0 := mux.Vars(r)["id0"]
		query := devices.Find(bson.M{"_id": id0})
		count, err := query.Count()
		if err != nil || count < 1 {
			log.Println(err)
			return
		}
		var device Device
		err = query.One(&device)
		if err != nil || device.HasMovable == false {
			w.Write([]byte("error"))
			return
		}
		buf := bytes.NewBuffer(make([]byte, 0, 0x140))
		_, err = buf.Write(device.MSed[:])
		if err != nil {
			log.Println(err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "inline; filename=\"movable.sed\"")
		w.Write(buf.Bytes())
	})
	// POST /upload/id0 w/ file movable and msed
	router.HandleFunc("/upload/{id0}", func(w http.ResponseWriter, r *http.Request) {
		id0 := mux.Vars(r)["id0"]
		file, header, err := r.FormFile("movable")
		if err != nil {
			log.Println(err)
			return
		}
		if header.Size != 0x120 && header.Size != 0x140 {
			w.WriteHeader(400)
			w.Write([]byte("error"))
			log.Println(header.Size)
			return
		}
		var movable [0x120]byte
		_, err = file.Read(movable[:])
		if err != nil {
			log.Println(err)
			return
		}

		// verify
		keyy := movable[0x110:0x11F]
		sha := sha256.Sum256(keyy)
		testid0 := fmt.Sprintf("%08x%08x%08x%08x", sha[0:4], sha[4:8], sha[8:12], sha[12:16])
		log.Println("id0check:", hex.EncodeToString(keyy), hex.EncodeToString(sha[:]), testid0, id0)

		err = devices.Update(bson.M{"_id": id0}, bson.M{"$set": bson.M{"msed": movable, "hasmovable": true, "expirytime": time.Time{}, "wantsbf": false}})
		if err != nil {
			log.Println(err)
			return
		}

		minerCollection.Upsert(bson.M{"_id": realip.FromRequest(r)}, bson.M{"$inc": bson.M{"score": 5}})

		for key, conn := range connections {
			if key == id0 {
				if err := conn.WriteMessage(websocket.TextMessage, buildMessage("done")); err != nil {
					log.Println(err)
					delete(connections, id0)
					//w.Write([]byte("fail"))
					//return
				}
			}
		}

		w.Write([]byte("success"))

		file2, header2, err := r.FormFile("msed")
		if header2.Size != 12 {
			log.Println(header.Size)
			return
		}
		var msed [12]byte
		_, err = file2.Read(msed[:])
		if err != nil {
			log.Println(err)
			return
		}
		log.Println(header2.Filename)
		filename := "msed_data_" + id0 + ".bin"
		err = ioutil.WriteFile("static/mseds/"+filename, msed[:], 0644)
		if err != nil {
			log.Println(err)
			return
		}
		f, err := os.OpenFile("static/mseds/list", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Println(err)
			return
		}
		_, err = f.WriteString(filename + "\n")
		if err != nil {
			log.Println(err)
			return
		}
		f.Close()

	}).Methods("POST")

	router.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renderTemplate("404error", make(jet.VarMap), r, w, nil)
	})

	// anti abuse task
	ticker := time.NewTicker(15 * time.Second)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				log.Println("running task")
				log.Println(miners)
				for ip, miner := range miners {
					if miner.Before(time.Now().Add(time.Minute*-5)) == true {
						delete(miners, ip)
					}
				}
				for ip, miner := range iminers {
					if miner.Before(time.Now().Add(time.Second*-30)) == true {
						delete(iminers, ip)
					}
				}
				query := devices.Find(bson.M{"wantsbf": true, "hasmovable": bson.M{"$ne": true}, "$or": []bson.M{bson.M{"claimtime": bson.M{"$lt": time.Now()}}, bson.M{"expirytime": bson.M{"$ne": time.Time{}, "$lt": time.Now()}}, bson.M{"expired": true}}})
				var theDevices []bson.M
				err := query.All(&theDevices)
				if err != nil {
					log.Println(err)
					//return
				}
				for _, device := range theDevices {
					if v, ok := device["checktime"].(time.Time); ok && v.After(time.Now()) {
						err = devices.Update(bson.M{"_id": device["_id"]}, bson.M{"$set": bson.M{"expirytime": time.Time{}, "wantsbf": false, "expired": true}})
						if err != nil {
							log.Println(err)
							//return
						}

						minerCollection.Upsert(bson.M{"_id": device["miner"]}, bson.M{"$inc": bson.M{"score": -3}})

						for id0, conn := range connections {
							if id0 == device["_id"] {
								if err := conn.WriteMessage(websocket.TextMessage, buildMessage("flag")); err != nil {
									log.Println(err)
									delete(connections, id0)
									//return
								}
							}
						}
						log.Println(device["_id"], "job has expired")

					} else {
						// checktime expired
						err = devices.Update(bson.M{"_id": device["_id"]}, bson.M{"$set": bson.M{"expirytime": time.Time{}}})
						if err != nil {
							log.Println(err)
							//return
						}

						for id0, conn := range connections {
							if id0 == device["_id"] {
								if err := conn.WriteMessage(websocket.TextMessage, buildMessage("queue")); err != nil {
									log.Println(err)
									delete(connections, id0)
									//return
								}
							}
						}
						log.Println(device["_id"], "job has checktimed")
					}
				}
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()

	log.Println("serving on :80 and 443")
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist("seedhelper.figgyc.uk"),
		Cache:      autocert.DirCache("."),
	}
	httpsSrv := &http.Server{
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  120 * time.Second,
		Handler:      router,
	}
	httpsSrv.Addr = ":443"
	httpsSrv.TLSConfig = &tls.Config{GetCertificate: m.GetCertificate}
	go http.ListenAndServe(":80", m.HTTPHandler(router))
	httpsSrv.ListenAndServeTLS("", "")
}
