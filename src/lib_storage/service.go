package lib_storage

import (
    "strings"
    "util/logger"
    "container/list"
    "strconv"
    "net"
    "util/pool"
    "util/file"
    "encoding/json"
    "time"
    "crypto/md5"
    "lib_common"
    "lib_common/header"
    "util/common"
    "hash"
    "app"
    "regexp"
    "io"
    "errors"
    "net/http"
    "util/db"
)


// max client connection set to 1000
var p, _ = pool.NewPool(1000, 100000)
// sys secret
var secret string
// sys config
var cfg map[string] string
// tasks put in this list
var fileRegisterList list.List

// Start service and listen
// 1. Start task for upload listen
// 2. Start task for communication with tracker
func StartService(config map[string] string) {
    cfg = config
    trackers := config["trackers"]
    port := config["port"]
    secret = config["secret"]

    // 连接数据库
    db.InitDB()

    startDownloadService()
    go startConnTracker(trackers)
    startUploadService(port)
}


// upload listen
func startUploadService(port string) {
    pt := lib_common.ParsePort(port)
    if pt > 0 {
        tryTimes := 0
        for {
            common.Try(func() {
                listener, e := net.Listen("tcp", ":" + strconv.Itoa(pt))
                logger.Info("service listening on port:", pt)
                if e != nil {
                    panic(e)
                } else {
                    // keep accept connections.
                    for {
                        conn, e1 := listener.Accept()
                        if e1 == nil {
                            ee := p.Exec(func() {
                                uploadHandler(conn)
                            })
                            // maybe the poll is full
                            if ee != nil {
                                lib_common.Close(conn)
                            }
                        } else {
                            logger.Info("accept new conn error", e1)
                            if conn != nil {
                                lib_common.Close(conn)
                            }
                        }
                    }
                }
            }, func(i interface{}) {
                logger.Error("["+ strconv.Itoa(tryTimes) +"] error shutdown service duo to:", i)
                time.Sleep(time.Second * 10)
            })
        }
    }
}

func startDownloadService() {

    if !app.HTTP_ENABLE {
        logger.Info("http server disabled")
        return
    }

    http.HandleFunc("/download/", DownloadHandler)

    s := &http.Server{
        Addr:           ":" + strconv.Itoa(app.HTTP_PORT),
        ReadTimeout:    10 * time.Second,
        WriteTimeout:   0,
        MaxHeaderBytes: 1 << 20,
    }
    logger.Info("http server listen on port:", app.HTTP_PORT)
    go s.ListenAndServe()
}

// communication with tracker
func startConnTracker(trackers string) {
    ls := parseTrackers(trackers)
    if ls.Len() == 0 {
        logger.Warn("no trackers set, the storage server will run in stand-alone mode.")
        return
    }

    for e := ls.Front(); e != nil; e = e.Next() {
        go onceConnTracker(e.Value.(string))
    }
}

// connect to each tracker
func onceConnTracker(tracker string) {
    logger.Info("start tracker conn with tracker server:", tracker)
    retry := 0
    for {//keep trying to connect to tracker server.
        conn, e := net.Dial("tcp", tracker)
        if e == nil {
            // validate client
            e1 := onConnectTrackerTask(conn)
            if e1 != nil {
                logger.Error("error keep connection with tracker server:", e1)
            } else {
                for { // keep sending client statistic info to tracker server.

                    logger.Debug("send info to tracker server")
                }
            }
        } else {
            logger.Error("(" + strconv.Itoa(retry) + ")error connect to tracker server:", tracker)
        }
        retry++
        time.Sleep(time.Second * app.REG_STORAGE_INTERVAL)
    }
}


func onConnectTrackerTask(conn net.Conn) error {
    regMeta := &header.CommunicationRegisterStorageRequestMeta{
        Secret: cfg["secret"],
        Group: cfg["group"],
        InstanceId: cfg["instance_id"],
        BindAddr: cfg["bind_address"],
        Port: lib_common.ParsePort(cfg["port"]),
    }
    metaSizeBytes, bodyBytes, metaStr, e1 := lib_common.PrepareMetaData(0, regMeta)
    if e1 != nil {
        logger.Error(e1)
        lib_common.Close(conn)
        return e1
    }
    e2 := lib_common.WriteMeta(0, metaSizeBytes, bodyBytes, []byte(metaStr), conn)
    if nil != e2 {
        lib_common.Close(conn)
        return e2
    }
    // read response
    _, respMeta, _, e3 := lib_common.ReadConnMeta(conn)
    if e3 != nil {
        lib_common.Close(conn)
        return e3
    }
    var resp = &header.CommunicationRegisterStorageResponseMeta{}
    e4 := json.Unmarshal([]byte(respMeta), resp)
    if e4 != nil {
        lib_common.Close(conn)
        return e4
    }
    logger.Debug("register response:", *resp)
    return lib_common.TranslateResponseStatus(resp.Status, conn)
}




// accept a new connection for file upload
// the connection will keep till it is broken
// 文件同步策略：
// 文件上传成功将任务写到本地文件storage_task.data作为备份
// 将任务通知到tracker服务器，通知成功，tracker服务进行广播
// 其他storage定时取任务，将任务
func uploadHandler(conn net.Conn) {

    defer func() {
        logger.Debug("close connection from server")
        conn.Close()
    }()

    common.Try(func() {
        // body buff
        bodyBuff := make([]byte, lib_common.BodyBuffSize)
        // calculate md5
        md := md5.New()

        feo := lib_common.CheckOnceOnConnect(conn)
        if feo != nil {
            return
        }

        for {
            // read meta
            operation, meta, bodySize, err := lib_common.ReadConnMeta(conn)
            if operation == -1 || meta == "" || err != nil {
                // otherwise mark as broken connection
                if err != nil {
                    logger.Error(err)
                }
                break
            }

            // client wants to upload file
            if operation == 2 {
                ee := operationUpload(bodySize, bodyBuff, md, conn)
                if ee != nil {
                    logger.Error("error read upload file:", ee)
                    break
                }
            } else if operation == 5 {// client wants to query file
                ee := operationQueryFile(meta, conn)
                if ee != nil {
                    logger.Error("error query file:", ee)
                    break
                }
            } else if operation == 6 {// client wants to download file
                ee := operationDownloadFile(meta, bodyBuff, conn)
                if ee != nil {
                    logger.Error("error query file:", ee)
                    break
                }
            }
        }
    }, func(i interface{}) {
        logger.Error("connection error:", i)
    })
}



// parse trackers into a list
func parseTrackers(tracker string) *list.List {
    sp := strings.Split(tracker, ",")
    ls := list.New()
    for i := range sp {
        trimS := strings.TrimSpace(sp[i])
        if len(trimS) > 0 {
            ls.PushBack(trimS)
        }

    }
    return ls
}



// 处理文件上传请求
func operationUpload(bodySize uint64, bodyBuff []byte, md hash.Hash, conn net.Conn) error {
    logger.Info("begin read file body, file len is ", bodySize/1024, "KB")
    // read file bytes
    e := lib_common.ReadConnBody(bodySize, bodyBuff, conn, md)
    if e != nil {
        return e
    }
    return nil
}


// 处理文件上传请求
func operationQueryFile(meta string, conn net.Conn) error {
    var queryMeta = &header.QueryFileRequestMeta{}
    e := json.Unmarshal([]byte(meta), queryMeta)
    if e != nil {
        var response = &header.UploadResponseMeta {
            Status: 3,
            Path: "",
            Exist: false,
        }
        lib_common.WriteResponse(4, conn, response)
        lib_common.Close(conn)
        return e
    }

    dig1 := queryMeta.Md5[0:2]
    dig2 := queryMeta.Md5[2:4]
    finalPath := app.BASE_PATH + "/data/" + dig1 + "/" + dig2 + "/" + queryMeta.Md5

    var response = &header.UploadResponseMeta{}
    if file.Exists(finalPath) {
        response = &header.UploadResponseMeta {
            Status: 0,
            Path: "",
            Exist: true,
        }
    } else {
        response = &header.UploadResponseMeta {
            Status: 0,
            Path: "",
            Exist: false,
        }
    }
    e1 := lib_common.WriteResponse(4, conn, response)
    if e1 != nil {
        lib_common.Close(conn)
        return e1
    }
    return nil
}

// 处理文件下载请求
func operationDownloadFile(meta string, buff []byte, conn net.Conn) error {
    var queryMeta = &header.DownloadFileRequestMeta{}
    e := json.Unmarshal([]byte(meta), queryMeta)
    if e != nil {
        var response = &header.UploadResponseMeta {
            Status: 3,
            Path: "",
            Exist: false,
        }
        lib_common.WriteResponse(4, conn, response)
        lib_common.Close(conn)
        return e
    }

    pathRegex := "^/([0-9a-zA-Z_]+)/([0-9a-zA-Z_]+)/([0-9a-fA-F]{32})$"
    if mat, _ := regexp.Match(pathRegex, []byte(queryMeta.Path)); !mat {
        var response = &header.UploadResponseMeta {
            Status: 0,
            Path: "",
            Exist: false,
        }
        e1 := lib_common.WriteResponse(4, conn, response)
        if e1 != nil {
            lib_common.Close(conn)
            return e1
        }
        return nil
    }
    //initialServer := regexp.MustCompile(pathRegex).ReplaceAllString("/x_/_123/432597de0e65eedbc867620e744a35ad", "${2}")
    md5 := regexp.MustCompile(pathRegex).ReplaceAllString(queryMeta.Path, "${3}")

    finalPath := GetFilePathByMd5(md5)

    var response = &header.UploadResponseMeta{}
    if file.Exists(finalPath) {
        response = &header.UploadResponseMeta {
            Status: 0,
            Path: "",
            Exist: true,
        }
        downFile, e1 := file.GetFile(finalPath)
        if e1 != nil {
            response.Status = 3
            response.Exist = false
            e1 := lib_common.WriteResponse(4, conn, response)
            if e1 != nil {
                lib_common.Close(conn)
                return e1
            }
            return nil
        }
        fInfo, _ := downFile.Stat()
        response.Status = 0
        response.Exist = true
        response.FileSize = fInfo.Size()
        e4 := lib_common.WriteResponse(4, conn, response)
        if e4 != nil {
            lib_common.Close(conn)
            return e4
        }
        for {
            len, e2 := downFile.Read(buff)
            if e2 == nil || e2 == io.EOF {
                wl, e5 := conn.Write(buff[0:len])
                if e2 == io.EOF {
                    downFile.Close()
                    return nil
                }
                if e5 != nil || wl != len {
                    downFile.Close()
                    lib_common.Close(conn)
                    return errors.New("error handle download file")
                }
            } else {
                downFile.Close()
                return e2
            }
        }
    } else {
        response = &header.UploadResponseMeta {
            Status: 0,
            Path: "",
            Exist: false,
        }
        e1 := lib_common.WriteResponse(4, conn, response)
        if e1 != nil {
            lib_common.Close(conn)
            return e1
        }
    }
    return nil
}

// check if file exists on the filesystem
func CheckIfFileExistByMd5(md5 string) bool {
    if file.Exists(GetFilePathByMd5(md5)) {
        return true
    }
    return false
}

// return file path using md5
func GetFilePathByMd5(md5 string) string {
    dig1 := strings.ToUpper(md5[0:2])
    dig2 := strings.ToUpper(md5[2:4])
    return app.BASE_PATH + "/data/" + dig1 + "/" + dig2 + "/" + md5
}
