package server

import (
	"fmt"
	"strings"
	"time"

	"github.com/chennqqi/godnslog/models"

	"github.com/chennqqi/godnslog/cache"
	"github.com/chennqqi/goutils/ginutils"
	"github.com/dgrijalva/jwt-go"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

type MyClaims struct {
	Seed string `json:"seed"`
	jwt.StandardClaims
}

//==============================================================================
// ui standard api
//==============================================================================
func (self *WebServer) respData(c *gin.Context, status, code int,
	message string, data interface{}) {
	c.JSON(status, &CR{
		Message:   message,
		Code:      code,
		Timestamp: time.Now().Unix(),
	})
}

func (self *WebServer) resp(c *gin.Context, status int, cr *CR) {
	cr.Timestamp = time.Now().Unix()
	c.JSON(status, cr)
}

func (self *WebServer) initDatabase() error {
	orm := self.orm
	orm.SetTZDatabase(time.Local)
	orm.SetTZLocation(time.Local)

	err := orm.Sync(&models.TblDns{}, &models.TblHttp{}, &models.TblUser{})
	if err != nil {
		logrus.Errorf("[webui.go::initDatabase] orm.Sync: %v", err)
		return err
	}
	count, err := orm.Count(&models.TblUser{})
	if err != nil {
		logrus.Errorf("[webui.go::initDatabase] orm.Count(user): %v", err)
		return err
	}
	if count == 0 {
		randomPass := genRandomString(12)
		_, err = orm.InsertOne(&models.TblUser{
			Name:          "admin",
			Email:         "admin@godnslog.com",
			ShortId:       genShortId(),
			Pass:          makePassword(randomPass),
			Token:         genRandomToken(),
			Role:          roleSuper,
			Lang:          self.DefaultLanguage,
			CleanInterval: self.DefaultCleanInterval,
		})
		if err != nil {
			logrus.Errorf("[webui.go::initDatabase] orm.InsertOne(user): %v", err)
			return err
		}
		fmt.Printf("Init super admin user with password: %v\n", randomPass)
	}

	store := self.store
	//sync user
	orm.Iterate(new(models.TblUser), func(idx int, bean interface{}) error {
		user := bean.(*models.TblUser)
		userKey := fmt.Sprintf("%v.user", user.Id)
		store.Set(userKey, user, cache.NoExpiration)
		domainKey := fmt.Sprintf("%v.suser", user.ShortId)
		store.Set(domainKey, user, cache.NoExpiration)
		return nil
	})

	return nil
}

func (self *WebServer) authHandler(c *gin.Context) {
	tokenString := c.GetHeader("Access-Token")
	if tokenString == "" {
		c.JSON(401, CR{
			Message: "Token Required",
			Code:    CodeNoAuth,
		})
		c.Abort()
		return
	}
	var claim MyClaims
	token, err := jwt.ParseWithClaims(tokenString, &claim, func(token *jwt.Token) (interface{}, error) {
		// since we only use the one private key to sign the tokens,
		// we also only use its public counter part to verify
		return []byte(self.verifyKey), nil
	})
	if token.Valid {
		store := self.store
		key := fmt.Sprintf("%v.seed", claim.Id)
		realSeed, exist := store.Get(key)
		if !exist {
			logrus.Infof("That's not even a token")
			c.JSON(401, CR{
				Message: "not login",
				Code:    CodeNoAuth,
			})
			c.Abort()
			return
		} else if realSeed.(string) != claim.Seed {
			logrus.Infof("That's not even a token")
			c.JSON(401, CR{
				Message: "Token Expire",
				Code:    CodeNoAuth,
			})
			c.Abort()
			return
		}
		u, exist := store.Get(fmt.Sprintf("%v.user", claim.Id))
		if !exist {
			logrus.Infof("[webui.go::authHandler] cache.Get(user), not exist")
			c.JSON(401, CR{
				Message: "not login",
				Code:    CodeNoAuth,
			})
			c.Abort()
			return
		}

		var uid int64
		fmt.Sscanf(claim.Id, "%d", &uid)
		c.Set("id", uid)
		c.Set("username", claim.Audience)
		c.Set("email", claim.Subject)
		c.Set("seed", claim.Seed)
		c.Set("role", u.(*models.TblUser).Role)

		//TODO: permission
		return
	} else if ve, ok := err.(*jwt.ValidationError); ok {
		if ve.Errors&jwt.ValidationErrorMalformed != 0 {
			logrus.Infof("That's not even a token")
			c.JSON(401, CR{
				Message: "Token invalid",
				Code:    CodeNoAuth,
			})
			c.Abort()
			return
		} else if ve.Errors&(jwt.ValidationErrorExpired|jwt.ValidationErrorNotValidYet) != 0 {
			// Token is either expired or not active yet
			logrus.Infof("Timing is everything")
			c.JSON(401, CR{
				Message: "Token Expired or not active yet",
				Code:    CodeNoAuth,
			})
			c.Abort()
			return
		} else {
			logrus.Warnf("Couldn't handle this token: %v", err)
			c.JSON(401, CR{
				Message: "Can't handle this token",
				Code:    0,
			})
			c.Abort()
			return
		}
	}
}

func (self *WebServer) verifyAdminPermission(c *gin.Context) {
	role := c.GetInt("role")
	switch role {
	case roleAdmin, roleSuper:
		return
	default:
		self.resp(c, 403, &CR{
			Message: "bad permission",
			Code:    CodeNoPermission,
		})
		c.Abort()
		return
	}
}

//==============================================================================
//									user auth
//==============================================================================

// @Summary userLogin
// @Description get Dns Record by user query
// @Accept  json
// @Produce  json
// @Param   some_id path int true "Some ID"
// @Success 200 {string} CR	"OK"
// @Failure 502 {object} CR "BadService"
// @Failure 403 {object} CR "Forbidden"
// @Failure 401 {object} CR "Unauthorized"
// @Router /user/login [post]
func (self *WebServer) userLogin(c *gin.Context) {
	var req LoginRequest
	err := c.BindJSON(&req)
	if err != nil {
		logrus.Infof("[webui.go::userLogin] bad input param")
		self.resp(c, 400, &CR{
			Code:    CodeBadData,
			Message: "bad input",
		})
		return
	}
	session := self.orm.NewSession()
	defer session.Close()
	var user = new(models.TblUser)
	exist, err := session.Where(`name=?`, req.Username).
		Or(`email=?`, req.Email).Get(user)

	if err != nil {
		logrus.Errorf("[webui.go::userLogin] orm.Get: %v", err)
		self.respData(c, 502, CodeServerInternal, "bad service", nil)
		return
	} else if !exist {
		logrus.Infof("[webui.go::userLogin] not found: %v", req)
		self.respData(c, 401, CodeBadData, "bad request", nil)
		return
	}
	err = comparePassword(req.Password, user.Pass)
	if err != nil {
		logrus.Infof("[webui.go::userLogin] password not match")
		self.respData(c, 401, CodeBadData, "bad request", nil)
		return
	}

	now := time.Now()
	seed := getSecuritySeed()
	token := jwt.NewWithClaims(jwt.SigningMethodHS384, MyClaims{
		seed,
		jwt.StandardClaims{
			Id:        fmt.Sprintf("%v", user.Id),
			Audience:  user.Name,
			Subject:   user.Email,
			ExpiresAt: now.Add(3600 * 24 * time.Second).Unix(),
			IssuedAt:  now.Unix(),
			Issuer:    self.Domain,
		},
	})

	tokenString, err := token.SignedString([]byte(self.verifyKey))
	if err != nil {
		logrus.Errorf("[webui.go::userLogin] token.SignedString: %v", err)

		self.respData(c, 502, CodeServerInternal, "bad service", nil)
		return
	}
	store := self.store

	store.Set(fmt.Sprintf("%v.seed", user.Id), seed, self.AuthExpire)
	store.Set(fmt.Sprintf("%v.user", user.Id), user, cache.NoExpiration)

	self.resp(c, 200, &CR{
		Message: "OK",
		Result: LoginResponse{
			Islogin: true,
			Token:   tokenString,
		},
	})
}

// @Summary userLogout
// @Description get Dns Record by user query
// @Accept  json
// @Produce  json
// @Param   some_id     path    int     true        "Some ID"
// @Success 200 {string} CR	"OK"
// @Failure 502 {object} CR "BadService"
// @Failure 403 {object} CR "Forbidden"
// @Failure 401 {object} CR "Unauthorized"
// @Router /user/logout [post]
func (self *WebServer) userLogout(c *gin.Context) {
	store := self.store
	id := c.GetInt64("id")
	store.Delete(fmt.Sprintf("%v.seed", id))
	store.Delete(fmt.Sprintf("%v.user", id))
	self.resp(c, 200, &CR{
		Message: "OK",
	})
}

// @Summary userInfo
// @Description get Dns Record by user query
// @Accept  json
// @Produce  json
// @Param   some_id     path    int     true        "Some ID"
// @Success 200 {string} string	"ok"
// @Failure 400 {object} CR "We need ID!!"
// @Failure 404 {object} CR "Can not find ID"
// @Failure 401 {object} CR "Can not find ID"
// @Router /user/info [get]
func (self *WebServer) userInfo(c *gin.Context) {
	id := c.GetInt64("id")
	session := self.orm.NewSession()
	defer session.Close()

	store := self.store
	userKey := fmt.Sprintf("%v.user", id)
	v, exist := store.Get(userKey)
	var user *models.TblUser
	if !exist {
		user = new(models.TblUser)
		exist, err := session.ID(id).Get(user)
		if err != nil {
			logrus.Errorf("[webui.go::userInfo] orm.Get: %v", err)
			self.resp(c, 502, &CR{
				Message: "Failed",
				Code:    CodeServerInternal,
			})
			return
		}
		if !exist {
			logrus.Errorf("[webui.go::userInfo] No such user")
			self.resp(c, 400, &CR{
				Message: "No such user",
				Code:    CodeBadData,
			})
			return
		}
		store.Set(userKey, user, cache.NoExpiration)
		domainKey := fmt.Sprintf("%v.suser", user.ShortId)
		store.Set(domainKey, user, cache.NoExpiration)
	} else {
		user = v.(*models.TblUser)
	}

	var role models.Role
	role.Id = "normal"
	role.Name = "用户"
	role.Permissions = []models.Permission{
		models.Permission{
			RoleId:         roleNormal,
			PermissionId:   "document",
			PermissionName: "文档",
		},
		models.Permission{
			RoleId:         roleNormal,
			PermissionId:   "record",
			PermissionName: "记录",
		},
	}
	switch user.Role {
	case roleAdmin, roleSuper:
		role.Id = "admin"
		role.Name = "管理员"
		role.Permissions = append(role.Permissions, []models.Permission{
			models.Permission{
				RoleId:         roleNormal,
				PermissionId:   "setting",
				PermissionName: "设置",
			},
			models.Permission{
				RoleId:         roleAdmin,
				PermissionId:   "manage",
				PermissionName: "管理用户",
			},
		}...)

	default:
		role.Permissions = append(role.Permissions, models.Permission{
			RoleId:         roleNormal,
			PermissionId:   "setting",
			PermissionName: "设置",
		})
	}

	//TODO: UserInfo from cache, role & permissions
	self.resp(c, 200, &CR{
		Message: "OK",
		Code:    CodeOK,
		Result: UserInfo{
			Id:    user.Id,
			Name:  user.Name,
			Email: user.Email,
			Role:  role,
		},
	})
}

// @Summary userInfo
// @Description get Dns Record by user query
// @Accept  json
// @Produce  json
// @Param   some_id     path    int     true        "Some ID"
// @Success 200 {string} string	"ok"
// @Failure 400 {object} CR "We need ID!!"
// @Failure 404 {object} CR "Can not find ID"
// @Failure 401 {object} CR "Can not find ID"
// @Router /admin/user/list [get]
func (self *WebServer) userList(c *gin.Context) {
	pageNo, pageNoErr := ginutils.GetQueryInt(c, "pageNo")
	if pageNoErr != nil || pageNo <= 0 {
		pageNo = 1
	}
	pageSize, pageSizeErr := ginutils.GetQueryInt(c, "pageSize")
	if pageSizeErr != nil {
		pageSize = 10
	}

	session := self.orm.NewSession()
	defer session.Close()

	session = session.Where(`id>1`)
	var items []models.TblUser
	count, err := session.Limit(pageSize, (pageNo-1)*pageSize).FindAndCount(&items)
	if err != nil {
		self.resp(c, 502, &CR{
			Code:    CodeServerInternal,
			Message: "Failed",
		})
		return
	}

	var resp UserListResp
	resp.TotalCount = int(count)
	resp.PageSize = pageSize
	resp.PageNo = pageNo
	resp.TotalPage = (resp.TotalCount + (pageSize - 1)) / pageSize
	resp.Data = make([]models.UserInfo, len(items))
	for i := 0; i < len(items); i++ {
		rcd := &resp.Data[i]
		item := &items[i]
		rcd.Id = item.Id
		rcd.Name = item.Name
		rcd.Email = item.Email
		rcd.Utime = item.Utime
		//TODO: others...
	}

	self.resp(c, 200, &CR{
		Message: "OK",
		Result:  &resp,
	})
}

// @Summary userNav
// @Description get Dns Record by user query
// @Accept  json
// @Produce  json
// @Param   some_id     path    int     true        "Some ID"
// @Success 200 {string} string	"ok"
// @Failure 400 {object} CR "We need ID!!"
// @Failure 404 {object} CR "Can not find ID"
// @Failure 401 {object} CR "Can not find ID"
// @Router /user/nav [get]
func (self *WebServer) userNav(c *gin.Context) {
}

//==============================================================================
//							user manage
//==============================================================================

// @Summary userNav
// @Description get Dns Record by user query
// @Accept  json
// @Produce  json
// @Param   some_id     path    int     true        "Some ID"
// @Success 200 {string} string	"ok"
// @Failure 400 {object} CR "We need ID!!"
// @Failure 404 {object} CR "Can not find ID"
// @Failure 401 {object} CR "Can not find ID"
// @Router /user/nav [get]
func (self *WebServer) delUser(c *gin.Context) {
	var req DeleteRecordRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		logrus.Infof("[webapi.go::delUser] parameter required")
		self.resp(c, 400, &CR{
			Message: "param required",
			Code:    CodeBadData,
		})
		return
	}
	var ids = make([]interface{}, len(req.Ids))
	for i := 0; i < len(req.Ids); i++ {
		ids[i] = req.Ids[i]
	}

	session := self.orm.NewSession()
	defer session.Close()

	//do not delete super user
	_, err = session.In("id", ids...).Delete(&models.TblUser{})
	if err != nil {
		logrus.Errorf("[webapi.go::delUser] orm.Delete: %v", err)
		self.resp(c, 502, &CR{
			Message: "failed",
			Code:    CodeServerInternal,
		})
		return
	}
	session.In("uid", ids).Delete(&models.TblDns{})
	session.In("uid", ids).Delete(&models.TblHttp{})

	cache := self.store
	for i := 0; i < len(req.Ids); i++ {
		seedKey := fmt.Sprintf("%v.seed", req.Ids[i])
		userKey := fmt.Sprintf("%v.user", req.Ids[i])
		v, exist := cache.Get(userKey)
		if exist {
			domainKey := fmt.Sprintf("%v.suser", v.(*models.TblUser).ShortId)
			cache.Delete(domainKey)
		}

		//logout these users
		cache.Delete(seedKey)
		cache.Delete(userKey)
	}

	self.resp(c, 200, &CR{
		Message: "OK",
	})
}

func (self *WebServer) addUser(c *gin.Context) {
	var req UserRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		logrus.Infof("[webui.go::addUser] parameter format invalid")
		self.resp(c, 400, &CR{
			Message: "Bad param",
			Code:    CodeBadData,
		})
		return
	}

	if isWeakPass(req.Password) {
		self.resp(c, 400, &CR{
			Message: "password too weak",
			Code:    CodeBadData,
		})
		return
	}

	//random api Token
	session := self.orm.NewSession()
	defer session.Close()

	var item = models.TblUser{
		Name:          req.Name,
		Email:         req.Email,
		Role:          roleNormal,
		Token:         genRandomToken(),
		ShortId:       genShortId(),
		Lang:          self.DefaultLanguage,
		Pass:          makePassword(req.Password),
		CleanInterval: self.DefaultCleanInterval,
	}
	_, err = session.InsertOne(&item)
	if self.IsDuplicate(err) {
		self.resp(c, 400, &CR{
			Message: "Failed",
			Code:    CodeBadData,
		})
		return
	} else if err != nil {
		self.resp(c, 502, &CR{
			Message: "Failed",
			Code:    CodeServerInternal,
		})
		return
	}
	self.resp(c, 200, &CR{
		Message: "OK",
	})
}

func (self *WebServer) setUser(c *gin.Context) {
	var req UserRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		logrus.Infof("[webapi.go::setUser] parameter required")
		self.resp(c, 400, &CR{
			Message: "param invaid: " + err.Error(),
			Code:    CodeBadData,
		})
		return
	}
	if req.Id < 1 {
		self.resp(c, 400, &CR{
			Message: "Can't change",
			Code:    CodeBadData,
		})
		return
	}

	store := self.store
	id := c.GetInt64("id")
	role := c.GetInt("role")
	session := self.orm.NewSession()
	defer session.Close()

	var user *models.TblUser

	switch role {
	case roleSuper, roleAdmin:
		//change other user
		session = session.ID(req.Id)
		if req.Password != "" {
			newPass := makePassword(req.Password)
			session = session.SetExpr(`pass`, customQuote(newPass))
		}
		if req.Language != "" {
			session = session.SetExpr(`lang`, customQuote(req.Language))
		}
		if req.Email != "" {
			session = session.SetExpr(`email`, customQuote(req.Email))
		}
		if req.Name != "" {
			session = session.SetExpr(`name`, customQuote(req.Name))
		}

		_, err = session.Update(&models.TblUser{})
		if err != nil {
			sql, _ := session.LastSQL()
			logrus.Errorf("[webapi.go::setUser] orm.Update error: %v, sql:%v", err, sql)
			self.resp(c, 400, &CR{
				Message: "failed",
				Code:    CodeServerInternal,
			})
			return
		}

		//logout req.Id
		cache := self.store
		cache.Delete(fmt.Sprintf("%v.seed", req.Id))
		cache.Delete(fmt.Sprintf("%v.user", req.Id))
		self.resp(c, 200, &CR{
			Message: "OK",
		})

	case roleNormal:
		//allow change language only
		userKey := fmt.Sprintf("%v.user", id)
		v, exist := store.Get(userKey)
		if !exist {
			user = new(models.TblUser)
			exist, err := session.ID(id).Get(user)
			if err != nil {
				sql, _ := session.LastSQL()
				logrus.Errorf("[webapi.go::setUser] orm.Get error: %v, sql:%v", err, sql)
				self.resp(c, 502, &CR{
					Message: "failed",
					Code:    CodeServerInternal,
				})
				return
			} else if !exist {
				//this should not happend
				self.resp(c, 400, &CR{
					Message: "Failed",
					Code:    CodeBadData,
				})
				return
			}

		} else {
			user = v.(*models.TblUser)
		}
		dupUser := new(models.TblUser)
		*dupUser = *user

		_, err := session.ID(id).Cols("lang").Update(dupUser)
		if err != nil {
			sql, _ := session.LastSQL()
			logrus.Errorf("[webapi.go::setUser] orm.Update error: %v, sql:%v", err, sql)
			self.resp(c, 400, &CR{
				Message: "failed",
				Code:    CodeServerInternal,
			})
			return
		}
		store.Set(userKey, dupUser, cache.NoExpiration)
		domainKey := fmt.Sprintf("%v.suser", dupUser.ShortId)
		store.Set(domainKey, dupUser, cache.NoExpiration)
	}
}

func (self *WebServer) getAppSetting(c *gin.Context) {
	id := c.GetInt64("id")
	store := self.store
	userKey := fmt.Sprintf("%v.user", id)
	v, exist := store.Get(fmt.Sprintf(userKey, id))
	var user *models.TblUser
	if !exist {
		session := self.orm.NewSession()
		defer session.Close()

		user = new(models.TblUser)
		exist, err := session.ID(id).Get(user)
		if err != nil {
			sql, _ := session.LastSQL()
			logrus.Errorf("[webui.go::getSecuritySetting] orm.Get error: %v, sql: %v", err, sql)
			self.resp(c, 502, &CR{
				Message: "Failed",
				Code:    CodeServerInternal,
			})
			return
		} else if !exist {
			logrus.Errorf("[webui.go::getSecuritySetting] not found user(id=%v), this should not happend", id)
			self.resp(c, 502, &CR{
				Message: "Failed",
				Code:    CodeServerInternal,
			})
			return
		}
		store.Set(userKey, user, cache.NoExpiration)
		domainKey := fmt.Sprintf("%v.suser", user.ShortId)
		store.Set(domainKey, user, cache.NoExpiration)
	} else {
		user = v.(*models.TblUser)
	}

	self.resp(c, 200, &CR{
		Message: "OK",
		Result: AppSetting{
			Rebind:    user.Rebind,
			Callback:  user.Callback,
			CleanHour: user.CleanInterval / 3600,
		},
	})
}

func (self *WebServer) setAppSetting(c *gin.Context) {
	var req AppSetting
	err := c.ShouldBindJSON(&req)
	if err != nil {
		logrus.Infof("[webui.go::setAppSetting] parameter format invalid")
		self.resp(c, 400, &CR{
			Message: "Bad param",
			Code:    CodeBadData,
		})
		return
	}

	id := c.GetInt64("id")
	store := self.store
	userKey := fmt.Sprintf("%v.user", id)
	v, exist := store.Get(userKey)
	session := self.orm.NewSession()
	defer session.Close()

	var user *models.TblUser
	if !exist {
		user = new(models.TblUser)
		exist, err := session.ID(id).Get(user)
		if err != nil {
			logrus.Errorf("[webuig.go::setAppSetting] orm.Get error: %v", err)
			self.resp(c, 502, &CR{
				Message: "Failed",
				Code:    CodeServerInternal,
			})
			return
		} else if !exist {
			logrus.Errorf("[webuig.go::setAppSetting] not found user(id=%v), this should not happend", id)
			self.resp(c, 502, &CR{
				Message: "Failed",
				Code:    CodeServerInternal,
			})
			return
		}
		store.Set(userKey, user, cache.NoExpiration)
		domainkey := fmt.Sprintf("%v.suser", user.ShortId)
		store.Set(domainkey, user, cache.NoExpiration)
	} else {
		user = v.(*models.TblUser)
	}

	dupUser := new(models.TblUser)
	*dupUser = *user
	dupUser.Rebind = req.Rebind
	dupUser.Callback = req.Callback
	dupUser.CleanInterval = req.CleanHour * 3600

	_, err = session.ID(id).Cols("rebind", "callback", "clean_iterval").Update(dupUser)
	if err != nil {
		logrus.Errorf("[webuig.go::setAppSetting] orm.Update error: %v", err)
		self.resp(c, 502, &CR{
			Message: "Failed",
			Code:    CodeServerInternal,
		})
		return
	}

	//update cache
	{
		domainKey := fmt.Sprintf("%v.suser", user.ShortId)
		userKey := fmt.Sprintf("%v.user", user.Id)
		store.Set(userKey, dupUser, cache.NoExpiration)
		store.Set(domainKey, dupUser, cache.NoExpiration)
	}

	self.resp(c, 200, &CR{
		Message: "OK",
	})
}

//change self password
func (self *WebServer) getSecuritySetting(c *gin.Context) {
	id := c.GetInt64("id")
	store := self.store
	userKey := fmt.Sprintf("%v.user", id)
	v, exist := store.Get(userKey)
	var user *models.TblUser
	if !exist {
		session := self.orm.NewSession()
		defer session.Close()
		user = new(models.TblUser)
		exist, err := session.ID(id).Get(user)
		if err != nil {
			sql, _ := session.LastSQL()
			logrus.Errorf("[webuig.go::getSecuritySetting] orm.Get error: %v, sql: %v", err, sql)
			self.resp(c, 502, &CR{
				Message: "Failed",
				Code:    CodeServerInternal,
			})
			return
		} else if !exist {
			logrus.Errorf("[webuig.go::getSecuritySetting] not found user(id=%v), this should not happend", id)
			self.resp(c, 502, &CR{
				Message: "Failed",
				Code:    CodeServerInternal,
			})
			return
		}
		store.Set(userKey, user, cache.NoExpiration)
		domainkey := fmt.Sprintf("%v.suser", user.ShortId)
		store.Set(domainkey, user, cache.NoExpiration)
	} else {
		user = v.(*models.TblUser)
	}

	self.resp(c, 200, &CR{
		Message: "OK",
		Result: AppSecurity{
			HttpAddr: fmt.Sprintf("https://%v/log/%v/", self.Domain, user.ShortId),
			DnsAddr:  user.ShortId + "." + self.Domain,
			Token:    user.Token,
		},
	})
}

//change self password
func (self *WebServer) setSecuritySetting(c *gin.Context) {
	var req AppSecuritySet
	err := c.ShouldBindJSON(&req)
	if err != nil {
		logrus.Infof("[webuig.go::setSecuritySetting] bad data")
		self.resp(c, 400, &CR{
			Message: "bad param",
			Code:    CodeBadData,
		})
		return
	}
	if isWeakPass(req.Password) {
		logrus.Warnf("[webuig.go::setSecuritySetting] weak password data")
		self.resp(c, 400, &CR{
			Message: "password too weak",
			Code:    CodeBadData,
		})
		return
	}

	id := c.GetInt64("id")
	session := self.orm.NewSession()
	defer session.Close()

	newPass := makePassword(req.Password)
	//logrus.Debugf("password:%v, hashpass=%v", req.Password, string(newPass))
	_, err = session.ID(id).SetExpr(`pass`, customQuote(newPass)).Update(&models.TblUser{})
	if err != nil {
		sql, _ := session.LastSQL()
		logrus.Errorf("[webuig.go::setSecuritySetting] orm.Update(%v), last SQL: %v", err, sql)
		self.resp(c, 502, &CR{
			Message: "update Failed",
			Code:    CodeServerInternal,
		})
		return
	}

	//logout & resp success
	self.userLogout(c)
}

//==============================================================================
// data api
//==============================================================================

// @Summary getDnsRecord
// @Description get Dns Record by user query
// @Accept  json
// @Produce  json
// @Param   some_id     path    int     true        "Some ID"
// @Success 200 {string} string	"ok"
// @Failure 400 {object} CR "We need ID!!"
// @Failure 404 {object} CR "Can not find ID"
// @Failure 401 {object} CR "Can not find ID"
// @Router /testapi/get-string-by-int/{some_id} [get]
func (self *WebServer) getDnsRecord(c *gin.Context) {
	ip, ipExist := c.GetQuery("ip")
	domain, domainExist := c.GetQuery("domain")
	date, dateExist := c.GetQuery("date")

	pageNo, pageNoErr := ginutils.GetQueryInt(c, "pageNo")
	if pageNoErr != nil || pageNo <= 0 {
		pageNo = 1
	}
	pageSize, pageSizeErr := ginutils.GetQueryInt(c, "pageSize")
	if pageSizeErr != nil {
		pageSize = 10
	}

	session := self.orm.NewSession()
	defer session.Close()

	role := c.GetInt("role")
	id := c.GetInt64("id")
	switch role {
	case roleAdmin, roleSuper:
		session = session.In("uid", 0, id)
	default:
		session = session.Where(`uid=?`, id)
	}

	if domainExist {
		session = session.And(`domain like ?`, "%"+domain+"%")
	}
	if ipExist {
		session = session.And(`ip like ?`, "%"+ip+"%")
	}
	if dateExist {
		t, _ := time.Parse(time.RFC3339, strings.Trim(date, `"`))
		if self.orm.DriverName() == "sqlite3" { //sqlite not support timezone
			t = t.Local()
		}
		session = session.And(`ctime > ?`, t)
		// fmt.Println("QUERYDATE=[", date, "] = ", t)
	}

	var items []models.TblDns
	count, err := session.Desc("id").Limit(pageSize, (pageNo-1)*pageSize).FindAndCount(&items)
	if err != nil {
		logrus.Errorf("[webui.go::getDnsRecord] orm.FindAndCount: %v", err)
		self.resp(c, 502, &CR{
			Message: "Failed",
			Code:    CodeServerInternal,
		})
		return
	}

	var resp DnsRecordResp
	resp.TotalCount = int(count)
	resp.PageSize = pageSize
	resp.PageNo = pageNo
	resp.TotalPage = (resp.TotalCount + (pageSize - 1)) / pageSize
	resp.Data = make([]models.DnsRecord, len(items))
	for i := 0; i < len(items); i++ {
		rcd := &resp.Data[i]
		item := &items[i]
		rcd.Id = item.Id
		rcd.Domain = item.Domain
		rcd.Ip = item.Ip
		rcd.Ctime = item.Ctime
	}

	self.resp(c, 200, &CR{
		Message: "OK",
		Result:  &resp,
	})
}

// @Summary delDnsRecord
// @Description del Dns Record by query ids
// @Accept  json
// @Produce  json
// @Param   some_id     path    int     true        "Some ID"
// @Success 200 {string} string	"ok"
// @Failure 400 {object} CR "We need ID!!"
// @Failure 404 {object} CR "Can not find ID"
// @Router /testapi/get-string-by-int/{some_id} [get]
func (self *WebServer) delDnsRecord(c *gin.Context) {
	var req DeleteRecordRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		logrus.Errorf("[webui.go::delDnsRecord] orm.Delete: %v", err)
		self.resp(c, 400, &CR{
			Message: "invalid Param",
			Code:    CodeServerInternal,
			Error:   err,
		})
		return
	}

	session := self.orm.NewSession()
	defer session.Close()

	role := c.GetInt("role")
	id := c.GetInt64("id")

	switch role {
	case roleAdmin, roleSuper:
		if len(req.Ids) == 0 {
			_, err := session.In(`uid`, id, 0).Delete(&models.TblDns{})
			if err != nil {
				//TODO:
				logrus.Errorf("[webui.go::delDnsRecord] orm.Delete: %v", err)
				self.resp(c, 502, &CR{
					Message: "Failed",
					Code:    CodeServerInternal,
				})
				return
			}
			self.resp(c, 200, &CR{
				Message: "OK",
			})
			return
		} else {
			params := make([]interface{}, len(req.Ids))
			for i := 0; i < len(req.Ids); i++ {
				params[i] = req.Ids[i]
			}
			_, err := session.In(`uid`, id, 0).In("id", params...).Delete(&models.TblDns{})
			if err != nil {
				logrus.Errorf("[webui.go::delDnsRecord] orm.Delete: %v", err)
				self.resp(c, 502, &CR{
					Message: "Failed",
					Code:    CodeServerInternal,
				})
				return
			}
			self.resp(c, 200, &CR{
				Message: "OK",
			})
			return
		}
	default:
		if len(req.Ids) == 0 {
			_, err := session.Where(`uid=?`, id).Delete(&models.TblDns{})
			if err != nil {
				logrus.Errorf("[webui.go::delDnsRecord] orm.Delete: %v", err)
				self.resp(c, 502, &CR{
					Message: "Failed",
					Code:    CodeServerInternal,
				})
				return
			}
			self.resp(c, 200, &CR{
				Message: "OK",
			})
			return
		} else {
			params := make([]interface{}, len(req.Ids))
			for i := 0; i < len(req.Ids); i++ {
				params[i] = req.Ids[i]
			}
			_, err := session.Where(`uid=?`, id).In("id", params...).Delete(&models.TblDns{})
			if err != nil {
				logrus.Errorf("[webui.go::delDnsRecord] orm.Delete: %v", err)
				self.resp(c, 502, &CR{
					Message: "Failed",
					Code:    CodeServerInternal,
				})
				return
			}
			self.resp(c, 200, &CR{
				Message: "OK",
			})
			return
		}
	}
}

func (self *WebServer) getHttpRecord(c *gin.Context) {
	ip, ipExist := c.GetQuery("ip")
	domain, domainExist := c.GetQuery("domain")
	date, dateExist := c.GetQuery("date")

	pageNo, pageNoErr := ginutils.GetQueryInt(c, "pageNo")
	if pageNoErr != nil || pageNo <= 0 {
		pageNo = 1
	}
	pageSize, pageSizeErr := ginutils.GetQueryInt(c, "pageSize")
	if pageSizeErr != nil || pageSize <= 0 {
		pageSize = 10
	}

	ctype, ctypeExist := c.GetQuery("ctype")
	data, dataExist := c.GetQuery("data")
	method, methodExist := c.GetQuery("method")

	session := self.orm.NewSession()
	defer session.Close()

	role := c.GetInt("role")
	id := c.GetInt64("id")
	switch role {
	case roleAdmin, roleSuper:
		session = session.Where(`id>0`)
	default:
		session = session.Where(`uid=?`, id)
	}

	if domainExist {
		session = session.And(`domain like ?`, "%"+domain+"%")
	}
	if ipExist {
		session = session.And(`ip like ?`, "%"+ip+"%")
	}
	if dateExist {
		t, _ := time.Parse(time.RFC3339, strings.Trim(date, `"`))
		if self.orm.DriverName() == "sqlite3" { //sqlite不支持时区
			t = t.Local()
		}
		session = session.And(`ctime > ?`, t)
	}
	if ctypeExist {
		session = session.And(`ctype like ?`, "%"+ctype+"%")
	}
	if dataExist {
		session = session.And(`data like ?`, "%"+data+"%")
	}
	if methodExist {
		session = session.And(`method = ?`, method)
	}

	var items []models.TblHttp
	count, err := session.Desc("id").Limit(pageSize, (pageNo-1)*pageSize).FindAndCount(&items)
	if err != nil {
		//TODO:
		logrus.Errorf("[webui.go::getHttpRecord] orm.FindAndCount: %v", err)
		self.resp(c, 502, &CR{
			Code:    CodeServerInternal,
			Message: "Faild",
		})
		return
	}

	var resp HttpRecordResp
	resp.TotalCount = int(count)
	resp.PageSize = pageSize
	resp.PageNo = pageNo
	resp.TotalPage = (resp.TotalCount + (pageSize - 1)) / pageSize
	resp.Data = make([]models.HttpRecord, len(items))

	for i := 0; i < len(items); i++ {
		rcd := &resp.Data[i]
		item := &items[i]
		rcd.Id = item.Id
		rcd.Path = item.Path
		rcd.Ip = item.Ip
		rcd.Ctime = item.Ctime
		rcd.Ctype = item.Ctype
		rcd.Data = item.Data
		rcd.Method = item.Method
		rcd.Ua = item.Ua
	}
	self.resp(c, 200, &CR{
		Message: "OK",
		Result:  &resp,
	})
}

func (self *WebServer) delHttpRecord(c *gin.Context) {
	var req DeleteRecordRequest
	err := c.ShouldBindJSON(&req)
	if err != nil {
		logrus.Errorf("[webui.go::delHttpRecord] orm.Delete: %v", err)
		self.resp(c, 400, &CR{
			Message: "invalid Param",
			Code:    CodeServerInternal,
			Error:   err,
		})
		return
	}

	session := self.orm.NewSession()
	defer session.Close()

	role := c.GetInt("role")
	id := c.GetInt64("id")

	switch role {
	case roleAdmin, roleSuper:
		if len(req.Ids) == 0 {
			_, err := session.In(`uid`, id, 0).Delete(&models.TblHttp{})
			if err != nil {
				//TODO:
				logrus.Errorf("[webui.go::delHttpRecord] orm.Delete: %v", err)
				self.resp(c, 502, &CR{
					Message: "Failed",
					Code:    CodeServerInternal,
				})
				return
			}
			self.resp(c, 200, &CR{
				Message: "OK",
			})
			return
		} else {
			params := make([]interface{}, len(req.Ids))
			for i := 0; i < len(req.Ids); i++ {
				params[i] = req.Ids[i]
			}
			_, err := session.In(`uid`, id, 0).In("id", params...).Delete(&models.TblHttp{})
			if err != nil {
				logrus.Errorf("[webui.go::delHttpRecord] orm.Delete: %v", err)
				self.resp(c, 502, &CR{
					Message: "Failed",
					Code:    CodeServerInternal,
				})
				return
			}
			self.resp(c, 200, &CR{
				Message: "OK",
			})
			return
		}
	default:
		if len(req.Ids) == 0 {
			_, err := session.Where(`uid=?`, id).Delete(&models.TblHttp{})
			if err != nil {
				//TODO:
				logrus.Errorf("[webui.go::delHttpRecord] orm.Delete: %v", err)
				self.resp(c, 502, &CR{
					Message: "Failed",
					Code:    CodeServerInternal,
				})
				return
			}
			self.resp(c, 200, &CR{
				Message: "OK",
			})
			return
		} else {
			params := make([]interface{}, len(req.Ids))
			for i := 0; i < len(req.Ids); i++ {
				params[i] = req.Ids[i]
			}
			_, err := session.Where(`uid=?`, id).In("id", params...).Delete(&models.TblHttp{})
			if err != nil {
				logrus.Errorf("[webui.go::delHttpRecord] orm.Delete: %v", err)
				self.resp(c, 502, &CR{
					Message: "Failed",
					Code:    CodeServerInternal,
				})
				return
			}
			self.resp(c, 200, &CR{
				Message: "OK",
			})
			return
		}
	}
}
