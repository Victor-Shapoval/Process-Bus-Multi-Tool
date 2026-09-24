//go:build mmsintegration

package mmsclient

/*
#include "iec61850_server.h"
#include "iec61850_dynamic_model.h"

typedef struct { IedModel* model; IedServer server; DataAttribute* state; } PBMTFixture;

static PBMTFixture fixture_start(int port) {
 PBMTFixture f;
 f.model = IedModel_create("TestIED");
 LogicalDevice* ld = LogicalDevice_create("LD0", f.model);
 LogicalNode* zero = LogicalNode_create("LLN0", ld);
 CDC_SPS_create("Loc", (ModelNode*) zero, 0);
 LogicalNode* ln = LogicalNode_create("GGIO1", ld);
 CDC_SPS_create("Ind1", (ModelNode*) ln, 0);
 CDC_MV_create("AnIn1", (ModelNode*) ln, 0, false);
 DataSet* ds = DataSet_create("Events", zero);
 DataSetEntry_create(ds, "GGIO1$ST$Ind1$stVal", -1, NULL);
 DataSetEntry_create(ds, "GGIO1$MX$AnIn1$mag$f", -1, NULL);
 ReportControlBlock_create("urcbEvents", zero, "TestURCB", false, "Events", 9,
     TRG_OPT_DATA_CHANGED | TRG_OPT_INTEGRITY | TRG_OPT_GI,
     RPT_OPT_SEQ_NUM | RPT_OPT_DATA_SET | RPT_OPT_CONF_REV, 25, 3000);
 ReportControlBlock_create("brcbEvents", zero, "TestBRCB", true, "Events", 10,
     TRG_OPT_QUALITY_CHANGED | TRG_OPT_DATA_UPDATE,
     RPT_OPT_TIME_STAMP | RPT_OPT_REASON_FOR_INCLUSION | RPT_OPT_DATA_REFERENCE | RPT_OPT_BUFFER_OVERFLOW | RPT_OPT_ENTRY_ID, 50, 5000);
 SettingGroupControlBlock_create(zero, 2, 4);
 GSEControlBlock* cb = GSEControlBlock_create("gcbEvents", zero, "TestGOOSE", "Events", 7, false, 4, 1000);
 uint8_t mac[6] = {1,12,205,1,0,2};
 GSEControlBlock_addPhyComAddress(cb, PhyComAddress_create(4, 0, 0x1001, mac));
 f.state = (DataAttribute*) IedModel_getModelNodeByObjectReference(f.model,"TestIEDLD0/GGIO1.Ind1.stVal");
 f.server = IedServer_create(f.model);
 IedServer_setLocalIpAddress(f.server, "127.0.0.1");
 IedServer_updateBooleanAttributeValue(f.server,f.state,true);
 IedServer_updateFloatAttributeValue(f.server,(DataAttribute*) IedModel_getModelNodeByObjectReference(f.model,"TestIEDLD0/GGIO1.AnIn1.mag.f"),12.5f);
 IedServer_start(f.server,port);
 return f;
}
static void fixture_stop(PBMTFixture f) { IedServer_stop(f.server); IedServer_destroy(f.server); IedModel_destroy(f.model); }
static void fixture_set(PBMTFixture f, bool v) { IedServer_updateBooleanAttributeValue(f.server,f.state,v); }
*/
import "C"

type fixture struct{ native C.PBMTFixture }

func startFixture(port int) *fixture { return &fixture{native: C.fixture_start(C.int(port))} }
func (f *fixture) running() bool     { return bool(C.IedServer_isRunning(f.native.server)) }
func (f *fixture) close()            { C.fixture_stop(f.native) }
func (f *fixture) set(value bool)    { C.fixture_set(f.native, C.bool(value)) }
